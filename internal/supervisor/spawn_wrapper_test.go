//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/execwrap"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

func startSupervisor(t *testing.T, us []*unit.Unit, configure func(*Supervisor)) *Supervisor {
	t.Helper()
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	d.Start(dispStop)
	sup, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if configure != nil {
		configure(sup)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	t.Cleanup(func() {
		sup.Stop()
		<-runDone
		close(dispStop)
		<-d.Done()
	})
	return sup
}

func waitSettled(t *testing.T, sup *Supervisor, names ...string) {
	t.Helper()
	for _, name := range names {
		select {
		case <-sup.ready[name]:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
}

// TestWorkingDirectoryIsEnteredByTheWrapper: the spawn itself starts in
// "/", and the service still runs in its WorkingDirectory=.
func TestWorkingDirectoryIsEnteredByTheWrapper(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "pwd")
	u := &unit.Unit{
		Name:             "wd.service",
		Kind:             unit.KindService,
		Type:             unit.TypeOneshot,
		WorkingDirectory: dir,
		ExecStart:        []string{"/bin/sh", "-c", "pwd > " + out},
	}
	sup := startSupervisor(t, []*unit.Unit{u}, nil)
	waitSettled(t, sup, u.Name)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("service did not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != dir {
		t.Errorf("service ran in %q, want %q", strings.TrimSpace(string(got)), dir)
	}
}

// TestStartFailuresReportedByTheWrapper: a missing WorkingDirectory=
// or ExecStart= binary is a start failure, not an ordinary non-zero
// exit.
func TestStartFailuresReportedByTheWrapper(t *testing.T) {
	dir := t.TempDir()
	badDir := &unit.Unit{
		Name:             "bad-dir.service",
		Kind:             unit.KindService,
		Type:             unit.TypeOneshot,
		WorkingDirectory: filepath.Join(dir, "missing"),
		ExecStart:        []string{"/bin/true"},
	}
	badExec := &unit.Unit{
		Name:      "bad-exec.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{filepath.Join(dir, "no-such-binary")},
	}
	sup := startSupervisor(t, []*unit.Unit{badDir, badExec}, nil)
	waitSettled(t, sup, badDir.Name, badExec.Name)
	sup.mu.Lock()
	defer sup.mu.Unlock()
	for name, code := range map[string]int{
		badDir.Name:  execwrap.ExitChdir,
		badExec.Name: execwrap.ExitExec,
	} {
		st := sup.services[name]
		if st == nil || !st.exited {
			t.Errorf("%s: wrapper not reaped", name)
			continue
		}
		if st.lastExit.ExitCode != code || !sup.failed[name] {
			t.Errorf("%s: exit %d failed=%v, want exit %d and failed", name, st.lastExit.ExitCode, sup.failed[name], code)
		}
	}
}

// TestStuckStartDoesNotBlockOtherSpawns stands in for a WorkingDirectory=
// on a hung mount: the wrapper never gets to exec the service. Other
// units must keep spawning and being reaped, and stop must still
// reach the stuck one.
func TestStuckStartDoesNotBlockOtherSpawns(t *testing.T) {
	dir := t.TempDir()
	// A wrapper that behaves like execwrap except for units with
	// STUCK=1 in their environment, which it leaves stuck before exec.
	// Recording its own cwd checks the spawn starts in "/".
	wrapper := filepath.Join(dir, "wrapper")
	script := `#!/bin/sh
# $1=__exec $2=status-fd $3=dir $4=path $5=-- then argv
pwd > "$3/.spawn-cwd"
if [ "$STUCK" = 1 ]; then exec sleep 300; fi
cd "$3" || exit 200
shift 5
exec "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stuckDir := filepath.Join(dir, "stuck")
	if err := os.Mkdir(stuckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stuck := &unit.Unit{
		Name:             "stuck.service",
		Kind:             unit.KindService,
		Type:             unit.TypeSimple,
		WorkingDirectory: stuckDir,
		Environment:      []string{"STUCK=1"},
		ExecStart:        []string{"/bin/sleep", "300"},
	}
	us := []*unit.Unit{stuck}
	var names []string
	for i := range 10 {
		wd := filepath.Join(dir, fmt.Sprintf("q%d", i))
		if err := os.Mkdir(wd, 0o755); err != nil {
			t.Fatal(err)
		}
		u := &unit.Unit{
			Name:             fmt.Sprintf("quick-%d.service", i),
			Kind:             unit.KindService,
			Type:             unit.TypeOneshot,
			WorkingDirectory: wd,
			ExecStart:        []string{"/bin/sh", "-c", "touch ran"},
		}
		us = append(us, u)
		names = append(names, u.Name)
	}
	sup := startSupervisor(t, us, func(s *Supervisor) { s.wrapper = wrapper })

	waitSettled(t, sup, names...)
	for i := range 10 {
		wd := filepath.Join(dir, fmt.Sprintf("q%d", i))
		if _, err := os.Stat(filepath.Join(wd, "ran")); err != nil {
			t.Errorf("quick-%d did not run while stuck.service was stuck: %v", i, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(stuckDir, ".spawn-cwd")); err != nil || strings.TrimSpace(string(b)) != "/" {
		t.Errorf("spawn cwd = %q (%v), want /", b, err)
	}
	select {
	case <-sup.ready[stuck.Name]:
		t.Error("stuck.service became ready without exec'ing its service")
	default:
	}

	// Stop must reach the stuck unit through its pid.
	sup.mu.Lock()
	pid := sup.services[stuck.Name].pid
	sup.mu.Unlock()
	sup.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Error("stuck wrapper survived stop")
	}
}

// TestExitStatusesUnderOrphanChurn: many services that exit at once,
// each with its own code, while another unit keeps leaving orphans for
// the dispatcher to reap as unknown PIDs. Every service must receive
// its own exit status.
func TestExitStatusesUnderOrphanChurn(t *testing.T) {
	// Make this process the reaper for orphans of its descendants, as
	// PID 1 is in production, so the churn reaches the dispatcher.
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Fatalf("PR_SET_CHILD_SUBREAPER: %v", errno)
	}
	t.Cleanup(func() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) })

	churn := &unit.Unit{
		Name:      "churn.service",
		Kind:      unit.KindService,
		Type:      unit.TypeSimple,
		ExecStart: []string{"/bin/sh", "-c", `while :; do sh -c 'sleep 0.01 &'; done`},
	}
	us := []*unit.Unit{churn}
	var names []string
	for i := 1; i <= 40; i++ {
		u := &unit.Unit{
			Name:      fmt.Sprintf("exit-%d.service", i),
			Kind:      unit.KindService,
			Type:      unit.TypeOneshot,
			After:     []string{"churn.service"},
			ExecStart: []string{"/bin/sh", "-c", fmt.Sprintf("exit %d", i)},
		}
		us = append(us, u)
		names = append(names, u.Name)
	}
	sup := startSupervisor(t, us, nil)
	waitSettled(t, sup, names...)

	sup.mu.Lock()
	defer sup.mu.Unlock()
	for i := 1; i <= 40; i++ {
		name := fmt.Sprintf("exit-%d.service", i)
		st := sup.services[name]
		if st == nil || !st.exited {
			t.Errorf("%s: not reaped", name)
			continue
		}
		if st.lastExit.ExitCode != i || st.lastExit.Pid != st.pid {
			t.Errorf("%s: got exit %d for pid %d (own pid %d), want exit %d",
				name, st.lastExit.ExitCode, st.lastExit.Pid, st.pid, i)
		}
	}
}

// TestWrapperRunsAsUnitUser: with User=, the credential switch happens
// before the wrapper runs, so the wrapper -- and then the service --
// run as that user. The wrapper binary must be executable by it, as
// /usr/local/bin/container-init is; the test binary lives in a private
// build dir, so run a world-executable copy of it.
func TestWrapperRunsAsUnitUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to switch user")
	}
	dir, err := os.MkdirTemp("", "wrapper-user-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "container-init")
	if err := os.WriteFile(wrapper, data, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "id")
	u := &unit.Unit{
		Name:             "as-nobody.service",
		Kind:             unit.KindService,
		Type:             unit.TypeOneshot,
		User:             "65534",
		Group:            "65534",
		WorkingDirectory: dir,
		ExecStart:        []string{"/bin/sh", "-c", `printf '%s %s' "$(id -u)" "$(pwd)" > id`},
	}
	sup := startSupervisor(t, []*unit.Unit{u}, func(s *Supervisor) { s.wrapper = wrapper })
	waitSettled(t, sup, u.Name)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("service did not run: %v", err)
	}
	if want := "65534 " + dir; string(got) != want {
		t.Errorf("service saw %q, want %q", got, want)
	}
}
