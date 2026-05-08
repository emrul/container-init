package unit

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeUnit(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := writeFile(filepath.Join(dir, name), body); err != nil {
		t.Fatal(err)
	}
}

func writeFile(path, body string) error {
	return osWriteFile(path, []byte(body), 0o644)
}

// indirection so tests don't pull os into the parse_test imports header.
var osWriteFile = func(path string, data []byte, perm uint32) error {
	return writeFileSyscall(path, data, perm)
}

func TestLoadDirRealisticFixture(t *testing.T) {
	dir := t.TempDir()
	// User= uses the ${VAR:-default} form the supervisor consumes.
	writeUnit(t, dir, "web.service", `[Unit]
Description=Web server (test fixture)

[Service]
Type=simple
User=${APP_USER:-app}
ExecStart=/bin/sleep 60
Restart=on-failure
RestartSec=200ms

[Install]
WantedBy=multi-user.target
`)
	writeUnit(t, dir, "wm.service", `[Unit]
Description=Window manager
After=web.service
Requires=web.service
OnFailure=drain.service

[Service]
Type=simple
ExecStart=/bin/sleep 30
Restart=on-failure
`)
	writeUnit(t, dir, "upload.socket", `[Unit]
Description=Upload listener

[Socket]
ListenStream=4902
ActivationMode=native
Service=upload.service
`)
	writeUnit(t, dir, "upload.service", `[Unit]
Description=Upload service
Requires=upload.socket

[Service]
Type=simple
ExecStart=/usr/bin/helper -mode native
`)
	writeUnit(t, dir, "audio.socket", `[Socket]
ListenStream=8081
ActivationMode=proxy
ProxyTarget=127.0.0.1:14081
Service=audio.service
`)
	writeUnit(t, dir, "audio.service", `[Service]
ExecStart=/usr/bin/helper -mode proxy -listen 127.0.0.1:14081
`)
	writeUnit(t, dir, "cond-skip.service", `[Unit]
ConditionEnvironment=FEATURE_OFF=1

[Service]
ExecStart=/bin/true
`)

	units, warnings, err := LoadDir(dir, Options{Lookup: func(k string) (string, bool) {
		// APP_USER is unset; default kicks in.
		// FEATURE_OFF is unset; cond-skip should be skipped.
		return "", false
	}})
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(units) != 7 {
		t.Fatalf("loaded %d units, want 7", len(units))
	}

	byName := map[string]*Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}

	// User=${APP_USER:-app} expands at parse time and lands on Unit.User.
	if got := byName["web.service"].User; got != "app" {
		t.Errorf("User default expansion = %q, want app", got)
	}

	wm := byName["wm.service"]
	if wm == nil {
		t.Fatalf("wm.service missing")
	}
	if wm.Restart != RestartOnFailure {
		t.Errorf("wm Restart = %v, want on-failure", wm.Restart)
	}
	if len(wm.OnFailure) != 1 || wm.OnFailure[0] != "drain.service" {
		t.Errorf("wm OnFailure = %v", wm.OnFailure)
	}

	up := byName["upload.socket"]
	if up == nil {
		t.Fatalf("upload.socket missing")
	}
	if up.ActivationMode != ActivationNative {
		t.Errorf("upload activation = %v", up.ActivationMode)
	}
	if len(up.ListenStream) != 1 || up.ListenStream[0].Network != "tcp" || up.ListenStream[0].Address != ":4902" {
		t.Errorf("upload listener = %#v", up.ListenStream)
	}
	if up.Service != "upload.service" {
		t.Errorf("upload Service = %q", up.Service)
	}

	au := byName["audio.socket"]
	if au == nil {
		t.Fatalf("audio.socket missing")
	}
	if au.ActivationMode != ActivationProxy || au.ProxyTarget != "127.0.0.1:14081" {
		t.Errorf("audio activation = %v target=%q", au.ActivationMode, au.ProxyTarget)
	}

	skip := byName["cond-skip.service"]
	if skip == nil || !skip.Condition.Skip {
		t.Errorf("cond-skip should be skipped, got %#v", skip)
	}
}

func TestExpansionFixture(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "u.service", `[Service]
ExecStart=/bin/echo ${APP_USER:-app}
`)
	// Default branch.
	units, _, err := LoadDir(dir, Options{Lookup: func(k string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("LoadDir default: %v", err)
	}
	if got := units[0].ExecStart; len(got) != 2 || got[1] != "app" {
		t.Errorf("default expansion ExecStart = %v", got)
	}
	// Override branch.
	units, _, err = LoadDir(dir, Options{Lookup: func(k string) (string, bool) {
		if k == "APP_USER" {
			return "alice", true
		}
		return "", false
	}})
	if err != nil {
		t.Fatalf("LoadDir override: %v", err)
	}
	if got := units[0].ExecStart; len(got) != 2 || got[1] != "alice" {
		t.Errorf("override expansion ExecStart = %v", got)
	}
}

// TestDirectiveSubset exercises every directive in the supported
// subset. One file, one directive each, ensuring values land on the
// typed field.
func TestDirectiveSubset(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "wide.service", `[Unit]
Description=wide
After=web.service wm.service
Before=watch.service
Requires=web.service
Wants=netwait.service
ConditionPathExists=/etc/passwd
ConditionPathExistsGlob=/dev/dri/*
ConditionEnvironment=FEATURE_X=1
OnFailure=drain.service

[Service]
Type=forking
ExecStartPre=/bin/mkdir -p /run/foo
ExecStartPre=/bin/chmod 0755 /run/foo
ExecStart=/usr/bin/foo --listen 127.0.0.1:4902
ExecStop=/bin/kill -s SIGTERM %n
ExecStopPost=/bin/rm -rf /run/foo
Restart=always
RestartSec=500ms
StartLimitBurst=5
StartLimitIntervalSec=10s
Environment=FOO=bar BAZ=qux
EnvironmentFile=/etc/foo.env
EnvironmentFile=-/etc/foo.optional.env
ExitContainerOnFailure=true
KillSignal=SIGINT
TimeoutStartSec=30
TimeoutStopSec=5s
RemainAfterExit=yes
PIDFile=/run/foo.pid
User=app
Group=app
WorkingDirectory=/home/app
`)
	writeUnit(t, dir, "wide.socket", `[Socket]
ListenStream=4902
ListenDatagram=/run/foo.dgram
SocketUser=root
SocketGroup=app
SocketMode=0660
ActivationMode=proxy
ProxyTarget=127.0.0.1:14902
Service=wide.service
`)

	units, warnings, err := LoadDir(dir, Options{Lookup: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	byName := map[string]*Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}

	svc := byName["wide.service"]
	if svc == nil {
		t.Fatal("wide.service missing")
	}
	// [Unit] checks
	if got, want := svc.After, []string{"web.service", "wm.service"}; !sliceEq(got, want) {
		t.Errorf("After = %v, want %v", got, want)
	}
	if got, want := svc.Before, []string{"watch.service"}; !sliceEq(got, want) {
		t.Errorf("Before = %v, want %v", got, want)
	}
	if got, want := svc.Wants, []string{"netwait.service"}; !sliceEq(got, want) {
		t.Errorf("Wants = %v, want %v", got, want)
	}
	if got, want := svc.ConditionPathExistsGlob, []string{"/dev/dri/*"}; !sliceEq(got, want) {
		t.Errorf("ConditionPathExistsGlob = %v, want %v", got, want)
	}
	// [Service] checks
	if svc.Type != TypeForking {
		t.Errorf("Type = %v, want forking", svc.Type)
	}
	if len(svc.ExecStartPre) != 2 {
		t.Errorf("ExecStartPre len = %d, want 2", len(svc.ExecStartPre))
	}
	if got, want := svc.ExecStartPre[1], []string{"/bin/chmod", "0755", "/run/foo"}; !sliceEq(got, want) {
		t.Errorf("ExecStartPre[1] = %v, want %v", got, want)
	}
	// %n in ExecStop= must have been expanded to "wide.service" at load time.
	if got, want := svc.ExecStop[0], []string{"/bin/kill", "-s", "SIGTERM", "wide.service"}; !sliceEq(got, want) {
		t.Errorf("ExecStop[0] = %v, want %v (specifier expansion)", got, want)
	}
	if got, want := svc.ExecStopPost[0], []string{"/bin/rm", "-rf", "/run/foo"}; !sliceEq(got, want) {
		t.Errorf("ExecStopPost[0] = %v, want %v", got, want)
	}
	if svc.Restart != RestartAlways {
		t.Errorf("Restart = %v, want always", svc.Restart)
	}
	if svc.StartLimitBurst != 5 {
		t.Errorf("StartLimitBurst = %d, want 5", svc.StartLimitBurst)
	}
	if svc.StartLimitIntervalSec != 10*time.Second {
		t.Errorf("StartLimitIntervalSec = %v, want 10s", svc.StartLimitIntervalSec)
	}
	if got, want := svc.Environment, []string{"FOO=bar", "BAZ=qux"}; !sliceEq(got, want) {
		t.Errorf("Environment = %v, want %v", got, want)
	}
	if len(svc.EnvironmentFile) != 2 {
		t.Errorf("EnvironmentFile len = %d, want 2", len(svc.EnvironmentFile))
	}
	if svc.EnvironmentFile[0] != (EnvFileRef{Path: "/etc/foo.env"}) {
		t.Errorf("EnvironmentFile[0] = %#v", svc.EnvironmentFile[0])
	}
	if svc.EnvironmentFile[1] != (EnvFileRef{Path: "/etc/foo.optional.env", IgnoreMissing: true}) {
		t.Errorf("EnvironmentFile[1] = %#v", svc.EnvironmentFile[1])
	}
	if !svc.ExitContainerOnFailure {
		t.Errorf("ExitContainerOnFailure = false, want true")
	}
	if svc.KillSignal != syscall.SIGINT {
		t.Errorf("KillSignal = %v, want SIGINT", svc.KillSignal)
	}
	if svc.TimeoutStartSec != 30*time.Second {
		t.Errorf("TimeoutStartSec = %v, want 30s", svc.TimeoutStartSec)
	}
	if svc.TimeoutStopSec != 5*time.Second {
		t.Errorf("TimeoutStopSec = %v, want 5s", svc.TimeoutStopSec)
	}
	if !svc.RemainAfterExit {
		t.Errorf("RemainAfterExit = false, want true")
	}
	if svc.PIDFile != "/run/foo.pid" {
		t.Errorf("PIDFile = %q", svc.PIDFile)
	}
	if svc.User != "app" || svc.Group != "app" || svc.WorkingDirectory != "/home/app" {
		t.Errorf("User/Group/WorkingDirectory = %q/%q/%q", svc.User, svc.Group, svc.WorkingDirectory)
	}

	sock := byName["wide.socket"]
	if sock == nil {
		t.Fatal("wide.socket missing")
	}
	if len(sock.ListenStream) != 1 || sock.ListenStream[0].Network != "tcp" {
		t.Errorf("ListenStream = %#v", sock.ListenStream)
	}
	if len(sock.ListenDatagram) != 1 || sock.ListenDatagram[0].Network != "unixgram" {
		t.Errorf("ListenDatagram = %#v", sock.ListenDatagram)
	}
	if sock.SocketUser != "root" || sock.SocketGroup != "app" {
		t.Errorf("SocketUser/Group = %q/%q", sock.SocketUser, sock.SocketGroup)
	}
	if sock.SocketMode != 0o660 {
		t.Errorf("SocketMode = %o, want 0660", sock.SocketMode)
	}
}

// TestUnknownDirectiveWarns verifies that directives outside the
// supported subset produce a warning naming the file + section +
// directive instead of failing the parse.
func TestUnknownDirectiveWarns(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "x.service", `[Unit]
Description=x
Documentation=https://example.invalid/

[Service]
Type=simple
ExecStart=/bin/true
PrivateNetwork=yes
NoNewPrivileges=yes

[Install]
Alias=x.service
`)
	units, warnings, err := LoadDir(dir, Options{})
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	wantDirectives := map[string]bool{
		"Documentation":   true,
		"PrivateNetwork":  true,
		"NoNewPrivileges": true,
		"Alias":           true,
	}
	got := map[string]bool{}
	for _, w := range warnings {
		got[w.Directive] = true
		if w.Path == "" {
			t.Errorf("warning missing path: %#v", w)
		}
		if !strings.Contains(w.String(), "x.service") {
			t.Errorf("warning string lacks filename: %s", w.String())
		}
	}
	for d := range wantDirectives {
		if !got[d] {
			t.Errorf("expected warning for directive %q, got %v", d, got)
		}
	}
}

// TestStrictModePromotesWarningsToError verifies --strict-units
// behaviour: an unknown directive that would warn becomes a fatal load
// error.
func TestStrictModePromotesWarningsToError(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "x.service", `[Service]
ExecStart=/bin/true
NoNewPrivileges=yes
`)
	_, _, err := LoadDir(dir, Options{Strict: true})
	if err == nil {
		t.Fatalf("strict LoadDir: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "strict-units") {
		t.Errorf("strict error message = %q, want contains 'strict-units'", err.Error())
	}
}

// TestSpecifierExpansion pins %n/%N/%H replacement at load time.
func TestSpecifierExpansion(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "spec.service", `[Unit]
Description=spec %n / %N / 100%%

[Service]
ExecStart=/bin/echo %n %N
`)
	units, _, err := LoadDir(dir, Options{})
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	u := units[0]
	if want := "spec spec.service / spec / 100%"; u.Description != want {
		t.Errorf("Description = %q, want %q", u.Description, want)
	}
	if got, want := u.ExecStart, []string{"/bin/echo", "spec.service", "spec"}; !sliceEq(got, want) {
		t.Errorf("ExecStart = %v, want %v", got, want)
	}
}

// TestBadValueIsError ensures that malformed values for known
// directives still fail the parse (warnings are reserved for *unknown*
// directives, not malformed ones — those produce immediate errors).
func TestBadValueIsError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "type",
			body: "[Service]\nType=quantum\nExecStart=/bin/true\n",
			want: "Type=",
		},
		{
			name: "restart",
			body: "[Service]\nExecStart=/bin/true\nRestart=sometimes\n",
			want: "Restart=",
		},
		{
			name: "killsignal",
			body: "[Service]\nExecStart=/bin/true\nKillSignal=SIGNOSUCH\n",
			want: "KillSignal",
		},
		{
			name: "socketmode",
			body: "[Socket]\nListenStream=4902\nSocketMode=99q9\nService=foo.service\n",
			want: "SocketMode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ext := ".service"
			if strings.Contains(tc.body, "[Socket]") {
				ext = ".socket"
			}
			writeUnit(t, dir, "x"+ext, tc.body)
			_, _, err := LoadDir(dir, Options{})
			if err == nil {
				t.Fatalf("expected parse error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want contains %q", err.Error(), tc.want)
			}
		})
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
