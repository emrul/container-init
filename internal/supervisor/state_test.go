//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// waitState waits until name's state satisfies ok, and returns it.
func waitState(t *testing.T, sup *Supervisor, name, what string, ok func(statefile.Unit) bool) statefile.Unit {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := sup.State()[name]
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never %s; state %+v", name, what, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func is(active, sub string) func(statefile.Unit) bool {
	return func(st statefile.Unit) bool { return st.Active == active && st.Sub == sub }
}

func wantState(t *testing.T, name string, st statefile.Unit, active, sub, result string, runs, restarts int) {
	t.Helper()
	if st.Active != active || st.Sub != sub || st.Result != result || st.Runs != runs || st.Restarts != restarts {
		t.Errorf("%s = %s/%s/%s runs %d restarts %d, want %s/%s/%s runs %d restarts %d",
			name, st.Active, st.Sub, st.Result, st.Runs, st.Restarts, active, sub, result, runs, restarts)
	}
}

func shService(name string, typ unit.ServiceType, script string) *unit.Unit {
	return &unit.Unit{Name: name, Kind: unit.KindService, Type: typ, ExecStart: []string{"/bin/sh", "-c", script}}
}

// Every loaded unit starts inactive/dead, not yet run, since New.
func TestStateStartsInactive(t *testing.T) {
	sock := socketFor("web.socket", "web.service")
	svc := shService("web.service", unit.TypeSimple, "exit 0")
	d := pid1.NewDispatcher()
	sup, err := New([]*unit.Unit{svc, sock}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range sup.State() {
		wantState(t, name, st, "inactive", "dead", "success", 0, 0)
		if !st.Since.Equal(sup.started) {
			t.Errorf("%s since %v, want PID 1's start %v", name, st.Since, sup.started)
		}
	}
	st := sup.State()
	if st["web.socket"].Type != "socket" || st["web.socket"].Service != "web.service" {
		t.Errorf("socket = %+v, want type socket naming web.service", st["web.socket"])
	}
	if got := st["web.service"].Sockets; len(got) != 1 || got[0] != "web.socket" {
		t.Errorf("service sockets = %v, want [web.socket]", got)
	}
	if st["web.service"].Type != "simple" {
		t.Errorf("service type = %q", st["web.service"].Type)
	}
}

// A oneshot is activating/start for its whole run; once done it is
// inactive/dead, or active/exited with RemainAfterExit=yes.
func TestStateOneshotLifecycle(t *testing.T) {
	for _, remain := range []bool{false, true} {
		t.Run(fmt.Sprintf("RemainAfterExit=%v", remain), func(t *testing.T) {
			dir := t.TempDir()
			gate := filepath.Join(dir, "go")
			u := shService("setup.service", unit.TypeOneshot,
				fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
			u.RemainAfterExit = remain
			sup := runUnits(t, u)
			st := waitState(t, sup, u.Name, "started", func(st statefile.Unit) bool { return st.Runs == 1 })
			time.Sleep(100 * time.Millisecond) // well into its run
			st = sup.State()[u.Name]
			wantState(t, u.Name, st, "activating", "start", "success", 1, 0)
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			active, sub := "inactive", "dead"
			if remain {
				active, sub = "active", "exited"
			}
			st = waitState(t, sup, u.Name, "finished", is(active, sub))
			wantState(t, u.Name, st, active, sub, "success", 1, 0)
		})
	}
}

// A Restart=on-failure service killed by a signal is restarted: runs
// and restarts go up by one, and it is active/running again.
func TestStateRestartAfterSignal(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	u := shService("daemon.service", unit.TypeSimple, fmt.Sprintf("echo $$ > '%s'; exec sleep 60", pidFile))
	u.Restart, u.RestartSec = unit.RestartOnFailure, 20*time.Millisecond
	sup := runUnits(t, u)
	st := waitState(t, sup, u.Name, "running", is("active", "running"))
	wantState(t, u.Name, st, "active", "running", "success", 1, 0)
	pid := waitPid(t, pidFile)
	_ = os.Remove(pidFile)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, sup, u.Name, "restarted", func(st statefile.Unit) bool {
		return st.Runs == 2 && st.Active == "active" && st.Sub == "running"
	})
	wantState(t, u.Name, st, "active", "running", "signal", 2, 1)
}

func waitPid(t *testing.T, f string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(f); err == nil {
			if pid, err := strconv.Atoi(string(b[:len(b)-1])); err == nil && len(b) > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", f)
	return 0
}

// Five crashes within the start limit: failed/start-limit-hit, five
// runs of which four were restarts.
func TestStateStartLimitHit(t *testing.T) {
	u := shService("crashy.service", unit.TypeSimple, "exit 3")
	u.Restart, u.RestartSec = unit.RestartOnFailure, time.Millisecond
	u.StartLimitBurst, u.StartLimitIntervalSec = 5, time.Minute
	sup := runUnits(t, u)
	st := waitState(t, sup, u.Name, "failed", is("failed", "failed"))
	wantState(t, u.Name, st, "failed", "failed", "start-limit-hit", 5, 4)
}

// A oneshot under Restart=always is never complete: after a clean exit
// it waits as activating/auto-restart with result success.
func TestStateOneshotRestartAlways(t *testing.T) {
	u := shService("loop.service", unit.TypeOneshot, "exit 0")
	u.Restart, u.RestartSec = unit.RestartAlways, time.Minute
	sup := runUnits(t, u)
	st := waitState(t, sup, u.Name, "waiting to restart", is("activating", "auto-restart"))
	wantState(t, u.Name, st, "activating", "auto-restart", "success", 1, 0)
}

// Failures that are not restarted: an exit code, and a start that
// fails before the process runs.
func TestStateFailedResults(t *testing.T) {
	exits := shService("exits.service", unit.TypeSimple, "exit 7")
	missing := &unit.Unit{Name: "missing.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{filepath.Join(t.TempDir(), "no-such-binary")}}
	sup := runUnits(t, exits, missing)
	st := waitState(t, sup, exits.Name, "failed", is("failed", "failed"))
	wantState(t, exits.Name, st, "failed", "failed", "exit-code", 1, 0)
	st = waitState(t, sup, missing.Name, "failed", is("failed", "failed"))
	wantState(t, missing.Name, st, "failed", "failed", "resources", 1, 0)
}

// An OnFailure= target waits untriggered; once its unit fails it runs
// once and finishes. A second trigger while it runs is dropped and not
// counted.
func TestStateOnFailureTarget(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	handler := shService("handler.service", unit.TypeOneshot,
		fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
	trigger := shService("trigger.service", unit.TypeSimple, "sleep 0.3; exit 1")
	trigger.OnFailure = []string{handler.Name}
	second := shService("second.service", unit.TypeSimple, "sleep 0.6; exit 1")
	second.OnFailure = []string{handler.Name}
	sup := runUnits(t, handler, trigger, second)
	st := sup.State()[handler.Name]
	if st.Activation != "on-failure" {
		t.Errorf("handler activation = %q, want on-failure", st.Activation)
	}
	wantState(t, handler.Name, st, "inactive", "dead", "success", 0, 0)
	waitState(t, sup, handler.Name, "triggered", is("activating", "start"))
	waitState(t, sup, second.Name, "failed", is("failed", "failed"))
	time.Sleep(100 * time.Millisecond) // the second trigger has been dropped
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, sup, handler.Name, "finished", is("inactive", "dead"))
	wantState(t, handler.Name, st, "inactive", "dead", "success", 1, 0)
}

// Units that will not start this boot say why.
func TestStateNever(t *testing.T) {
	broken := shService("broken.service", unit.TypeOneshot, "exit 1")
	needy := shService("needy.service", unit.TypeSimple, "sleep 60")
	needy.Requires, needy.After = []string{broken.Name}, []string{broken.Name}
	orphan := shService("orphan.service", unit.TypeSimple, "sleep 60")
	orphan.Requires = []string{"absent.service"}
	skipped := shService("skipped.service", unit.TypeSimple, "sleep 60")
	skipped.Condition.Skip, skipped.Condition.Reason = true, "test"
	sup := runUnits(t, broken, needy, orphan, skipped)
	for name, want := range map[string]string{
		needy.Name: "dependency", orphan.Name: "missing-requirement", skipped.Name: "condition",
	} {
		st := waitState(t, sup, name, "marked never", func(st statefile.Unit) bool { return st.Never != "" })
		if st.Never != want {
			t.Errorf("%s never = %q, want %q", name, st.Never, want)
		}
		wantState(t, name, st, "inactive", "dead", "success", 0, 0)
	}
}

// A socket listens from boot, its service untriggered; a connection
// runs the service, and after a clean exit the socket still listens.
func TestStateSocketActivation(t *testing.T) {
	dir := t.TempDir()
	sock, svc := socketPair(t, unit.ActivationNative, dir, "echo")
	count := filepath.Join(dir, "count")
	helperService(t, sock, svc, count, 0)
	sup := runUnits(t, sock, svc)
	waitListening(t, sup, sock.Name)
	st := sup.State()
	wantState(t, sock.Name, st[sock.Name], "active", "listening", "success", 1, 0)
	wantState(t, svc.Name, st[svc.Name], "inactive", "dead", "success", 0, 0)
	if st[svc.Name].Activation != "socket" {
		t.Errorf("service activation = %q, want socket", st[svc.Name].Activation)
	}
	request(t, sock.ListenStream[0].Address)
	waitIdle(t, sup, svc.Name, count, 1)
	svcSt := waitState(t, sup, svc.Name, "finished", is("inactive", "dead"))
	wantState(t, svc.Name, svcSt, "inactive", "dead", "success", 1, 0)
	wantState(t, sock.Name, sup.State()[sock.Name], "active", "listening", "success", 1, 0)
}

// A socket whose service keeps failing past its start limit fails with
// service-start-limit-hit.
func TestStateSocketServiceStartLimit(t *testing.T) {
	dir := t.TempDir()
	sock, svc := socketPair(t, unit.ActivationNative, dir, "flaky")
	count := filepath.Join(dir, "count")
	helperService(t, sock, svc, count, 1)
	svc.Restart, svc.RestartSec = unit.RestartOnFailure, time.Millisecond
	svc.StartLimitBurst, svc.StartLimitIntervalSec = 2, time.Minute
	sup := runUnits(t, sock, svc)
	waitListening(t, sup, sock.Name)
	// Each run serves one connection and fails; the third start is
	// past the limit.
	request(t, sock.ListenStream[0].Address)
	request(t, sock.ListenStream[0].Address)
	st := waitState(t, sup, sock.Name, "failed", is("failed", "failed"))
	wantState(t, sock.Name, st, "failed", "failed", "service-start-limit-hit", 1, 0)
	wantState(t, svc.Name, sup.State()[svc.Name], "failed", "failed", "start-limit-hit", 2, 1)
}

// A socket that cannot bind fails with resources; its service will
// never start.
func TestStateSocketBindFailure(t *testing.T) {
	dir := t.TempDir()
	sock, svc := socketPair(t, unit.ActivationNative, dir, "nobind")
	sock.ListenStream[0].Address = filepath.Join(dir, "missing-dir", "sock")
	helperService(t, sock, svc, filepath.Join(dir, "count"), 0)
	sup := runUnits(t, sock, svc)
	st := waitState(t, sup, sock.Name, "failed", is("failed", "failed"))
	wantState(t, sock.Name, st, "failed", "failed", "resources", 0, 0)
	st = waitState(t, sup, svc.Name, "marked never", func(st statefile.Unit) bool { return st.Never != "" })
	if st.Never != "dependency" {
		t.Errorf("service never = %q, want dependency", st.Never)
	}
}

// Reverse shutdown leaves stopped units inactive/dead, keeps failed
// ones failed, and does not record the stop as a failed run.
func TestStateAfterShutdown(t *testing.T) {
	running := shService("running.service", unit.TypeSimple, "exec sleep 60")
	failed := shService("failed.service", unit.TypeSimple, "exit 2")
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)
	sup, err := New([]*unit.Unit{running, failed}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	sup.SetStopTimeout(2 * time.Second)
	done := make(chan struct{})
	go func() { sup.Run(); close(done) }()
	waitState(t, sup, running.Name, "running", is("active", "running"))
	waitState(t, sup, failed.Name, "failed", is("failed", "failed"))
	sup.Stop()
	<-done
	st := sup.State()
	wantState(t, running.Name, st[running.Name], "inactive", "dead", "success", 1, 0)
	wantState(t, failed.Name, st[failed.Name], "failed", "failed", "exit-code", 1, 0)
}

// Every change signals the state writer, without blocking the
// lifecycle when nobody reads the signal.
func TestStateChangesSignalWithoutBlocking(t *testing.T) {
	u := shService("quick.service", unit.TypeOneshot, "exit 0")
	sup := runUnits(t, u)
	waitState(t, sup, u.Name, "finished", func(st statefile.Unit) bool {
		return st.Runs == 1 && st.Active == "inactive"
	})
	select {
	case <-sup.changed:
	default:
		t.Fatal("no change signalled")
	}
}

// The start plan is in the first report: a skipped service's socket is
// skipped too, and what starts each unit is known before Run.
func TestStatePlanKnownAtNew(t *testing.T) {
	skippedSvc := shService("off.service", unit.TypeSimple, "exit 0")
	skippedSvc.Condition.Skip, skippedSvc.Condition.Reason = true, "test"
	skippedSock := socketFor("off.socket", "off.service")
	handler := shService("handler.service", unit.TypeOneshot, "exit 0")
	failing := shService("failing.service", unit.TypeSimple, "exit 1")
	failing.OnFailure = []string{handler.Name}
	sup, err := New([]*unit.Unit{skippedSvc, skippedSock, handler, failing}, nil, pid1.NewDispatcher(), &cgroup.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	st := sup.State()
	for _, name := range []string{"off.service", "off.socket"} {
		if st[name].Never != "condition" || st[name].Activation != "" {
			t.Errorf("%s = never %q activation %q, want never condition, no activation", name, st[name].Never, st[name].Activation)
		}
	}
	if st["handler.service"].Activation != "on-failure" {
		t.Errorf("handler activation = %q, want on-failure", st["handler.service"].Activation)
	}
	if st["failing.service"].Activation != "" || st["failing.service"].Never != "" {
		t.Errorf("failing.service = %+v, want neither activation nor never", st["failing.service"])
	}
}
