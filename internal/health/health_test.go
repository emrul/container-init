package health

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/statefile"
)

var now = time.Date(2026, 9, 29, 17, 5, 0, 0, time.UTC)

func at(t time.Time) statefile.Time { return statefile.Time{Time: t} }

// unit states, by the design's words.
var (
	running   = statefile.Unit{Type: "simple", Active: "active", Sub: "running", Result: "success", Runs: 1}
	crashed   = statefile.Unit{Type: "simple", Active: "failed", Sub: "failed", Result: "start-limit-hit", Runs: 5, Restarts: 4}
	restartng = statefile.Unit{Type: "simple", Active: "activating", Sub: "auto-restart", Result: "signal", Runs: 2, Restarts: 1}
	oneRun    = statefile.Unit{Type: "oneshot", Active: "activating", Sub: "start", Result: "success", Runs: 1}
	oneDone   = statefile.Unit{Type: "oneshot", Active: "inactive", Sub: "dead", Result: "success", Runs: 1}
	oneKept   = statefile.Unit{Type: "oneshot", Active: "active", Sub: "exited", Result: "success", Runs: 1}
	oneNotRun = statefile.Unit{Type: "oneshot", Active: "inactive", Sub: "dead", Result: "success"}
	oneLoop   = statefile.Unit{Type: "oneshot", Active: "activating", Sub: "auto-restart", Result: "success", Runs: 3, Restarts: 2}
	never     = statefile.Unit{Type: "simple", Active: "inactive", Sub: "dead", Result: "success", Never: "dependency"}
	listening = statefile.Unit{Type: "socket", Active: "active", Sub: "listening", Result: "success", Runs: 1}
	sockFail  = statefile.Unit{Type: "socket", Active: "failed", Sub: "failed", Result: "service-start-limit-hit", Runs: 1}
	onFailIdl = statefile.Unit{Type: "oneshot", Active: "inactive", Sub: "dead", Result: "success", Activation: "on-failure"}
	onFailRun = statefile.Unit{Type: "oneshot", Active: "activating", Sub: "start", Result: "success", Runs: 1, Activation: "on-failure"}
	onFailEnd = statefile.Unit{Type: "oneshot", Active: "inactive", Sub: "dead", Result: "success", Runs: 1, Activation: "on-failure"}
	onFailBad = statefile.Unit{Type: "oneshot", Active: "failed", Sub: "failed", Result: "exit-code", Runs: 1, Activation: "on-failure"}
)

func sock(u statefile.Unit, service string) statefile.Unit { u.Service = service; return u }

func activated(u statefile.Unit, sockets ...string) statefile.Unit {
	u.Activation, u.Sockets = "socket", sockets
	return u
}

func file(units map[string]statefile.Unit) *statefile.File {
	return &statefile.File{Version: 1, Written: at(now.Add(-5 * time.Second)),
		PID1: statefile.PID1{Version: "vtest", Started: at(now.Add(-time.Minute))}, Units: units}
}

func TestCheck(t *testing.T) {
	idleSvc := activated(oneNotRun, "upload.socket")
	idleSvc.Type = "simple"
	svcFailed := activated(statefile.Unit{Type: "simple", Active: "failed", Sub: "failed", Result: "exit-code", Runs: 1}, "upload.socket")
	svcUp := activated(running, "upload.socket")
	svcIdleAfterRun := activated(statefile.Unit{Type: "simple", Active: "inactive", Sub: "dead", Result: "success", Runs: 1}, "upload.socket")
	cases := []struct {
		name    string
		units   map[string]statefile.Unit
		require []string
		healthy bool
		why     string // a substring of the reason
	}{
		{"no require, nothing failed", map[string]statefile.Unit{"a.service": running, "b.service": oneRun}, nil, true, "none failed"},
		{"no require, one failed", map[string]statefile.Unit{"a.service": running, "b.service": crashed}, nil, false, "b.service"},
		{"optional unit failed", map[string]statefile.Unit{"a.service": running, "b.service": crashed}, []string{"a.service"}, true, ""},
		{"required service running", map[string]statefile.Unit{"a.service": running}, []string{"a.service"}, true, ""},
		{"required service restarting", map[string]statefile.Unit{"a.service": restartng}, []string{"a.service"}, false, "auto-restart"},
		{"required service failed", map[string]statefile.Unit{"a.service": crashed}, []string{"a.service"}, false, "start-limit-hit"},
		{"required service never", map[string]statefile.Unit{"a.service": never}, []string{"a.service"}, false, "never starting: dependency"},
		{"required unit missing", map[string]statefile.Unit{"a.service": running}, []string{"b.service"}, false, "b.service is not loaded"},
		{"oneshot running", map[string]statefile.Unit{"s.service": oneRun}, []string{"s.service"}, false, "activating/start"},
		{"oneshot finished", map[string]statefile.Unit{"s.service": oneDone}, []string{"s.service"}, true, ""},
		{"oneshot RemainAfterExit", map[string]statefile.Unit{"s.service": oneKept}, []string{"s.service"}, true, ""},
		{"oneshot not run", map[string]statefile.Unit{"s.service": oneNotRun}, []string{"s.service"}, false, "has not run"},
		{"oneshot Restart=always", map[string]statefile.Unit{"s.service": oneLoop}, []string{"s.service"}, false, "auto-restart"},
		{"socket before first connection", map[string]statefile.Unit{
			"upload.socket": sock(listening, "upload.service"), "upload.service": idleSvc,
		}, []string{"upload.socket", "upload.service"}, true, ""},
		{"socket service idle after a clean run", map[string]statefile.Unit{
			"upload.socket": sock(listening, "upload.service"), "upload.service": svcIdleAfterRun,
		}, []string{"upload.socket", "upload.service"}, true, ""},
		{"socket service up", map[string]statefile.Unit{
			"upload.socket": sock(listening, "upload.service"), "upload.service": svcUp,
		}, []string{"upload.socket"}, true, ""},
		{"socket listening, service failed: require the socket", map[string]statefile.Unit{
			"upload.socket": sock(listening, "upload.service"), "upload.service": svcFailed,
		}, []string{"upload.socket"}, false, "upload.socket's service"},
		{"socket listening, service failed: require the service", map[string]statefile.Unit{
			"upload.socket": sock(listening, "upload.service"), "upload.service": svcFailed,
		}, []string{"upload.service"}, false, "failed/failed"},
		{"socket failed: require the service", map[string]statefile.Unit{
			"upload.socket": sock(sockFail, "upload.service"), "upload.service": activated(crashed, "upload.socket"),
		}, []string{"upload.service"}, false, "upload.service"},
		{"socket failed, service up: require the service", map[string]statefile.Unit{
			"upload.socket": sock(sockFail, "upload.service"), "upload.service": svcUp,
		}, []string{"upload.service"}, false, "socket upload.socket is failed"},
		{"socket failed: require the socket", map[string]statefile.Unit{
			"upload.socket": sock(sockFail, "upload.service"), "upload.service": idleSvc,
		}, []string{"upload.socket"}, false, "service-start-limit-hit"},
		{"socket's service not loaded", map[string]statefile.Unit{
			"upload.socket": sock(listening, "gone.service"),
		}, []string{"upload.socket"}, false, "gone.service, which is not loaded"},
		{"socket names a service of another stem", map[string]statefile.Unit{
			"files.socket": sock(listening, "uploader.service"), "uploader.service": activated(oneNotRun, "files.socket"),
		}, []string{"files.socket", "uploader.service"}, true, ""},
		{"two proxy sockets, one failed", map[string]statefile.Unit{
			"a.socket": sock(listening, "svc.service"), "b.socket": sock(sockFail, "svc.service"),
			"svc.service": activated(svcUp, "a.socket", "b.socket"),
		}, []string{"svc.service"}, false, "socket b.socket"},
		{"on-failure target not triggered", map[string]statefile.Unit{"h.service": onFailIdl}, []string{"h.service"}, true, ""},
		{"on-failure target running", map[string]statefile.Unit{"h.service": onFailRun}, []string{"h.service"}, false, "activating"},
		{"on-failure target finished", map[string]statefile.Unit{"h.service": onFailEnd}, []string{"h.service"}, true, ""},
		{"on-failure target failed", map[string]statefile.Unit{"h.service": onFailBad}, []string{"h.service"}, false, "exit-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := Check(file(tc.units), now, tc.require, DefaultMaxAge)
			if ok != tc.healthy || !strings.Contains(why, tc.why) {
				t.Errorf("Check = %v %q, want %v containing %q", ok, why, tc.healthy, tc.why)
			}
		})
	}
}

func TestCheckStaleAndStopping(t *testing.T) {
	f := file(map[string]statefile.Unit{"a.service": running})
	f.Written = at(now.Add(-31 * time.Second))
	if ok, why := Check(f, now, nil, DefaultMaxAge); ok || !strings.Contains(why, "stale") {
		t.Errorf("stale file: %v %q", ok, why)
	}
	if ok, _ := Check(f, now, nil, time.Minute); !ok {
		t.Error("--max-age 1m rejected a 31s-old heartbeat")
	}
	f = file(map[string]statefile.Unit{"a.service": running})
	f.PID1.Stopping = true
	if ok, why := Check(f, now, []string{"a.service"}, DefaultMaxAge); ok || !strings.Contains(why, "shutting down") {
		t.Errorf("stopping: %v %q", ok, why)
	}
}

// Main exits 0 or 1, never 2, on every outcome, with one line saying
// why.
func TestMain(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if _, err := statefile.Write(good, file(map[string]statefile.Unit{"a.service": running, "b.service": crashed})); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--state-file", good, "--require", "a.service"}, 0, "healthy:"},
		{[]string{"--state-file", good, "--require", "a.service,b.service"}, 1, "b.service"},
		{[]string{"--state-file", good, "--require", "a.service", "--require", "b.service"}, 1, "b.service"},
		{[]string{"--state-file", good}, 1, "failed: b.service"},
		{[]string{"--state-file", filepath.Join(dir, "absent.json")}, 1, "no such file"},
		{[]string{"--state-file", bad}, 1, "unhealthy:"},
		{[]string{}, 1, "usage"},
		{[]string{"--bogus"}, 1, ""},
		{[]string{"--state-file", good, "extra"}, 1, "usage"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		code := Main(tc.args, &out, now)
		if code != tc.code || !strings.Contains(out.String(), tc.out) {
			t.Errorf("health %v = %d %q, want %d containing %q", tc.args, code, out.String(), tc.code, tc.out)
		}
		if strings.Count(strings.TrimSpace(out.String()), "\n") > 0 && tc.out != "" {
			t.Errorf("health %v printed more than one line: %q", tc.args, out.String())
		}
	}
}
