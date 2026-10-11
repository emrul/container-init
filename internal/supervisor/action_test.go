//go:build linux

package supervisor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// runToExit starts us and waits up to wait for Run to return on its
// own. exited is false if it did not; the supervisor is then stopped,
// and code is the stopped run's.
func runToExit(t *testing.T, wait time.Duration, us ...*unit.Unit) (sup *Supervisor, code int, exited bool) {
	t.Helper()
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer func() { close(dispStop); <-d.Done() }()
	d.Start(dispStop)
	sup, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan int, 1)
	go func() { done <- sup.Run() }()
	select {
	case code = <-done:
		return sup, code, true
	case <-time.After(wait):
	}
	sup.Stop()
	select {
	case code = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
	return sup, code, false
}

// exitingUnit is a Type=simple service that runs script under restart.
func exitingUnit(name, script string, restart unit.RestartPolicy) *unit.Unit {
	u := shellUnit(name, script)
	u.Restart = restart
	u.RestartSec = 10 * time.Millisecond
	return u
}

func wantExit(t *testing.T, sup *Supervisor, unitName, action, result string, status int) {
	t.Helper()
	e := sup.stateExit()
	if e == nil {
		t.Fatalf("pid1.exit not recorded, want %s of %s", action, unitName)
	}
	if e.Unit != unitName || e.Action != action || e.Result != result {
		t.Errorf("pid1.exit = %s %s %s, want %s %s %s", e.Unit, e.Action, e.Result, unitName, action, result)
	}
	switch {
	case status < 0 && e.Status != nil:
		t.Errorf("pid1.exit.status = %d, want absent", *e.Status)
	case status >= 0 && (e.Status == nil || *e.Status != status):
		t.Errorf("pid1.exit.status = %v, want %d", e.Status, status)
	}
}

// TestSuccessActionOnCleanExit: a service that exits 0 and is not
// restarted is inactive, and SuccessAction=exit takes the container
// down with its exit code.
func TestSuccessActionOnCleanExit(t *testing.T) {
	u := exitingUnit("a.service", "exit 0", unit.RestartNo)
	u.SuccessAction = unit.ActionExit
	sup, code, exited := runToExit(t, 5*time.Second, u)
	if !exited || code != 0 {
		t.Fatalf("exited=%v code=%d, want the container to exit 0", exited, code)
	}
	wantExit(t, sup, "a.service", "SuccessAction=exit", statefile.ResultSuccess, 0)
}

// TestSuccessActionExitStatus: SuccessActionExitStatus= replaces the
// unit's exit code, here for Restart=on-failure, which does not restart
// a clean exit.
func TestSuccessActionExitStatus(t *testing.T) {
	u := exitingUnit("a.service", "exit 0", unit.RestartOnFailure)
	u.SuccessAction = unit.ActionExit
	u.SuccessActionExitStatus, u.SuccessActionExitStatusSet = 5, true
	sup, code, exited := runToExit(t, 5*time.Second, u)
	if !exited || code != 5 {
		t.Fatalf("exited=%v code=%d, want the container to exit 5", exited, code)
	}
	wantExit(t, sup, "a.service", "SuccessAction=exit", statefile.ResultSuccess, 5)
}

// TestSuccessActionNotOnRestart: with Restart=always a clean exit is
// restarted, so the unit never becomes inactive and SuccessAction=
// does not fire, as in systemd.
func TestSuccessActionNotOnRestart(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	u := exitingUnit("a.service", countingScript(count, "exit 0"), unit.RestartAlways)
	u.SuccessAction = unit.ActionExit
	u.FailureAction = unit.ActionExit
	sup, _, exited := runToExit(t, time.Second, u)
	if exited {
		t.Fatal("the container exited, want the unit restarted")
	}
	if n := readCount(count); n < 2 {
		t.Errorf("unit ran %d times, want it restarted", n)
	}
	if e := sup.stateExit(); e != nil {
		t.Errorf("pid1.exit = %+v, want none", *e)
	}
}

// TestFailureActionPropagatesExitCode: a failed unit's exit code is the
// container's.
func TestFailureActionPropagatesExitCode(t *testing.T) {
	u := exitingUnit("a.service", "exit 7", unit.RestartNo)
	u.FailureAction = unit.ActionExit
	sup, code, exited := runToExit(t, 5*time.Second, u)
	if !exited || code != 7 {
		t.Fatalf("exited=%v code=%d, want the container to exit 7", exited, code)
	}
	wantExit(t, sup, "a.service", "FailureAction=exit", statefile.ResultExitCode, 7)
}

// TestFailureActionOnSignal: a unit killed by a signal has no exit code
// to pass on: the container exits as a normal shutdown would, unless
// FailureActionExitStatus= says otherwise.
func TestFailureActionOnSignal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int // -1: unset
		want   int
	}{
		{"default", -1, 0},
		{"FailureActionExitStatus", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := exitingUnit("a.service", "kill -9 $$", unit.RestartNo)
			u.FailureAction = unit.ActionExit
			if tc.status >= 0 {
				u.FailureActionExitStatus, u.FailureActionExitStatusSet = tc.status, true
			}
			sup, code, exited := runToExit(t, 5*time.Second, u)
			if !exited || code != tc.want {
				t.Fatalf("exited=%v code=%d, want the container to exit %d", exited, code, tc.want)
			}
			wantExit(t, sup, "a.service", "FailureAction=exit", statefile.ResultSignal, tc.status)
		})
	}
}

// TestFailureActionAfterRestartsRunOut: with Restart=on-failure,
// FailureAction= waits for the unit to fail for good -- here at its
// start limit -- not for its first failure.
func TestFailureActionAfterRestartsRunOut(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	u := exitingUnit("a.service", countingScript(count, "exit 1"), unit.RestartOnFailure)
	u.StartLimitBurst, u.StartLimitIntervalSec = 3, time.Minute
	u.FailureAction = unit.ActionExit
	u.FailureActionExitStatus, u.FailureActionExitStatusSet = 3, true
	sup, code, exited := runToExit(t, 5*time.Second, u)
	if !exited || code != 3 {
		t.Fatalf("exited=%v code=%d, want the container to exit 3", exited, code)
	}
	if n := readCount(count); n != 3 {
		t.Errorf("unit ran %d times, want 3 (its start limit)", n)
	}
	wantExit(t, sup, "a.service", "FailureAction=exit", statefile.ResultStartLimitHit, 3)
}

// TestFailureActionNoneByDefault: without an action a failed unit
// leaves the container running.
func TestFailureActionNoneByDefault(t *testing.T) {
	u := exitingUnit("a.service", "exit 1", unit.RestartNo)
	sup, _, exited := runToExit(t, 500*time.Millisecond, u)
	if exited {
		t.Fatal("the container exited, want it running")
	}
	if e := sup.stateExit(); e != nil {
		t.Errorf("pid1.exit = %+v, want none", *e)
	}
}

// TestSuccessActionOneshot: a oneshot is inactive once it exits 0, so
// SuccessAction= fires; with RemainAfterExit= it stays active, and does
// not.
func TestSuccessActionOneshot(t *testing.T) {
	for _, remain := range []bool{false, true} {
		u := &unit.Unit{
			Name:            "once.service",
			Kind:            unit.KindService,
			Type:            unit.TypeOneshot,
			ExecStart:       []string{"/bin/true"},
			RemainAfterExit: remain,
			SuccessAction:   unit.ActionExit,
		}
		_, _, exited := runToExit(t, time.Second, u)
		if exited == remain {
			t.Errorf("RemainAfterExit=%v: exited=%v, want %v", remain, exited, !remain)
		}
	}
}

// TestActionsNotOnShutdown: a unit stopped by reverse shutdown is
// being stopped, not finishing: neither action fires, and the exit
// status is shutdown's own.
func TestActionsNotOnShutdown(t *testing.T) {
	up := filepath.Join(t.TempDir(), "up")
	u := shellUnit("a.service", "touch '"+up+"'; exec sleep 60")
	u.SuccessAction = unit.ActionExit
	u.FailureAction = unit.ActionExit
	u.SuccessActionExitStatus, u.SuccessActionExitStatusSet = 9, true
	u.FailureActionExitStatus, u.FailureActionExitStatusSet = 9, true
	code, _ := runAndStop(t, 5*time.Second, []string{up}, u)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// TestExitContainerOnFailureStatus: ExitContainerOnFailure= still fires
// on the first failure, ahead of Restart=, and the container now exits
// with the unit's exit code, or FailureActionExitStatus=.
func TestExitContainerOnFailureStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		status int // -1: unset
		want   int
		result string
	}{
		{"exit code", "exit 4", -1, 4, statefile.ResultExitCode},
		{"FailureActionExitStatus", "exit 4", 3, 3, statefile.ResultExitCode},
		{"signal", "kill -9 $$", 3, 3, statefile.ResultSignal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := filepath.Join(t.TempDir(), "count")
			u := exitingUnit("a.service", countingScript(count, tc.script), unit.RestartAlways)
			u.ExitContainerOnFailure = true
			if tc.status >= 0 {
				u.FailureActionExitStatus, u.FailureActionExitStatusSet = tc.status, true
			}
			sup, code, exited := runToExit(t, 5*time.Second, u)
			if !exited || code != tc.want {
				t.Fatalf("exited=%v code=%d, want the container to exit %d", exited, code, tc.want)
			}
			if n := readCount(count); n != 1 {
				t.Errorf("unit ran %d times, want 1", n)
			}
			wantExit(t, sup, "a.service", "ExitContainerOnFailure=", tc.result, tc.want)
		})
	}
}

// TestExitForceKillsAtOnce: exit-force does not wait for units to stop:
// one ignoring SIGTERM is killed at once rather than after its
// TimeoutStopSec=.
func TestExitForceKillsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		action  unit.EmergencyAction
		minWait time.Duration
		maxWait time.Duration
	}{
		{unit.ActionExit, 2 * time.Second, 10 * time.Second},
		{unit.ActionExitForce, 0, 1500 * time.Millisecond},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			up := filepath.Join(t.TempDir(), "up")
			stubborn := shellUnit("stubborn.service", ignoringTerm(up))
			stubborn.TimeoutStopSec = 2 * time.Second
			// Fails once stubborn is up, so stubborn is there to stop.
			trigger := exitingUnit("trigger.service",
				"while [ ! -f '"+up+"' ]; do sleep 0.05; done; exit 6", unit.RestartNo)
			trigger.FailureAction = tc.action
			begin := time.Now()
			_, code, exited := runToExit(t, 15*time.Second, stubborn, trigger)
			took := time.Since(begin)
			if !exited || code != 6 {
				t.Fatalf("exited=%v code=%d, want the container to exit 6", exited, code)
			}
			if took < tc.minWait || took > tc.maxWait {
				t.Errorf("took %v, want between %v and %v", took, tc.minWait, tc.maxWait)
			}
		})
	}
}

// TestSuccessActionSocketActivated: a socket-activated service that
// exits 0 is inactive like any other, so its SuccessAction= fires, as
// in systemd; without one, the socket listens again.
func TestSuccessActionSocketActivated(t *testing.T) {
	for _, action := range []unit.EmergencyAction{unit.ActionNone, unit.ActionExit} {
		t.Run(action.String(), func(t *testing.T) {
			dir := t.TempDir()
			sock, svc := socketPair(t, unit.ActivationNative, dir, "echo")
			helperService(t, sock, svc, filepath.Join(dir, "count"), 0)
			svc.SuccessAction = action
			d := pid1.NewDispatcher()
			dispStop := make(chan struct{})
			defer func() { close(dispStop); <-d.Done() }()
			d.Start(dispStop)
			sup, err := New([]*unit.Unit{sock, svc}, nil, d, &cgroup.Manager{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			done := make(chan int, 1)
			go func() { done <- sup.Run() }()
			defer func() {
				sup.Stop()
				<-done
			}()
			waitFile(t, filepath.Join(dir, "sock"))
			request(t, filepath.Join(dir, "sock"))
			wait := time.Second // long enough for a wrong exit to show
			if action != unit.ActionNone {
				wait = 5 * time.Second
			}
			select {
			case code := <-done:
				done <- code // for the deferred wait
				if action == unit.ActionNone {
					t.Fatalf("the container exited (%d), want the socket listening again", code)
				}
				wantExit(t, sup, "echo.service", "SuccessAction=exit", statefile.ResultSuccess, 0)
			case <-time.After(wait):
				if action != unit.ActionNone {
					t.Fatal("the container did not exit")
				}
			}
		})
	}
}
