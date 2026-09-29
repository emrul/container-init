//go:build linux

package supervisor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

// shellUnit is a Type=simple service running script, which should
// create its up file first.
func shellUnit(name, script string, after ...string) *unit.Unit {
	return &unit.Unit{
		Name:      name,
		Kind:      unit.KindService,
		Type:      unit.TypeSimple,
		After:     after,
		ExecStart: []string{"/bin/sh", "-c", script},
	}
}

// runAndStop starts us, waits for every up file, stops the supervisor
// and returns Run's exit code and how long the shutdown took.
func runAndStop(t *testing.T, stopTimeout time.Duration, ups []string, us ...*unit.Unit) (int, time.Duration) {
	t.Helper()
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)
	sup, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sup.SetStopTimeout(stopTimeout)
	exit := make(chan int, 1)
	go func() { exit <- sup.Run() }()

	deadline := time.Now().Add(5 * time.Second)
	for _, f := range ups {
		for {
			if _, err := os.Stat(f); err == nil {
				break
			}
			if time.Now().After(deadline) {
				sup.Stop()
				<-exit
				t.Fatalf("%s never appeared", f)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	begin := time.Now()
	sup.Stop()
	select {
	case code := <-exit:
		return code, time.Since(begin)
	case <-time.After(20 * time.Second):
		t.Fatal("supervisor did not shut down")
		return 0, 0
	}
}

// TestShutdownInReverseOrder: a unit is stopped only once the units
// ordered After= it have stopped, however long they take.
func TestShutdownInReverseOrder(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")
	stopper := func(name, delay string) string {
		return fmt.Sprintf(`trap 'sleep %s; echo %s >> %s; exit 0' TERM; touch %s; sleep 60 & wait`,
			delay, name, order, filepath.Join(dir, name+".up"))
	}
	// c is ordered after b, b after a: the slowest to stop comes first.
	a := shellUnit("a.service", stopper("a", "0"))
	b := shellUnit("b.service", stopper("b", "0.2"), "a.service")
	c := shellUnit("c.service", stopper("c", "0.4"), "b.service")
	ups := []string{filepath.Join(dir, "a.up"), filepath.Join(dir, "b.up"), filepath.Join(dir, "c.up")}

	code, _ := runAndStop(t, 5*time.Second, ups, a, b, c)
	data, _ := os.ReadFile(order)
	if got := strings.Fields(string(data)); strings.Join(got, " ") != "c b a" {
		t.Errorf("stop order = %v, want [c b a]", got)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// ignoringTerm is a script that ignores SIGTERM and creates up.
func ignoringTerm(up string) string {
	return fmt.Sprintf(`trap '' TERM; touch %s; while :; do sleep 0.05; done`, up)
}

// TestShutdownHonoursTimeoutStopSec: a unit that ignores its stop
// signal is killed after its own TimeoutStopSec=, not the default.
func TestShutdownHonoursTimeoutStopSec(t *testing.T) {
	up := filepath.Join(t.TempDir(), "up")
	u := shellUnit("stubborn.service", ignoringTerm(up))
	u.TimeoutStopSec = 300 * time.Millisecond

	code, took := runAndStop(t, 5*time.Second, []string{up}, u)
	if took < 300*time.Millisecond || took > 2*time.Second {
		t.Errorf("shutdown took %v, want about 300ms", took)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (force-killed)", code)
	}
}

// TestShutdownStopTimeoutBoundsTheWhole: the stop timeout cuts every
// unit's wait short, including the waits for units ordered after it.
func TestShutdownStopTimeoutBoundsTheWhole(t *testing.T) {
	dir := t.TempDir()
	upA, upB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	a := shellUnit("a.service", ignoringTerm(upA))
	b := shellUnit("b.service", ignoringTerm(upB), "a.service")
	a.TimeoutStopSec = 10 * time.Second
	b.TimeoutStopSec = 10 * time.Second

	code, took := runAndStop(t, 500*time.Millisecond, []string{upA, upB}, a, b)
	if took > 2*time.Second {
		t.Errorf("shutdown took %v, want about 500ms", took)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (force-killed)", code)
	}
}

// TestShutdownSendsKillSignal: a unit is stopped with its KillSignal=.
func TestShutdownSendsKillSignal(t *testing.T) {
	dir := t.TempDir()
	up, got := filepath.Join(dir, "up"), filepath.Join(dir, "sig")
	u := shellUnit("int.service", fmt.Sprintf(
		`trap 'echo int > %[1]s; exit 0' INT; trap 'echo term > %[1]s; exit 0' TERM; touch %[2]s; sleep 60 & wait`,
		got, up))
	u.KillSignal = syscall.SIGINT

	code, _ := runAndStop(t, 5*time.Second, []string{up}, u)
	if data, _ := os.ReadFile(got); strings.TrimSpace(string(data)) != "int" {
		t.Errorf("unit got %q, want int", strings.TrimSpace(string(data)))
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// TestShutdownDoesNotFireOnFailure: a unit that dies of its stop signal
// is being stopped, not failing, so its OnFailure= is not invoked (nor
// logged as failed).
func TestShutdownDoesNotFireOnFailure(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	up, mark := filepath.Join(dir, "up"), filepath.Join(dir, "fired")
	u := shellUnit("dies.service", fmt.Sprintf(`touch %s; exec sleep 60`, up))
	u.OnFailure = []string{"drain.service"}
	drain := &unit.Unit{
		Name:      "drain.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{"/bin/touch", mark},
	}

	runAndStop(t, 5*time.Second, []string{up}, u, drain)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Error("OnFailure= ran during shutdown")
	}
	for _, line := range []string{"OnFailure: invoking", "unit dies.service: failed"} {
		if strings.Contains(logs.String(), line) {
			t.Errorf("log has %q during shutdown:\n%s", line, logs.String())
		}
	}
}

// TestSocketClosesAfterItsDependentsStop: a socket's listener stays
// open while a unit ordered After= it is still stopping, in both modes;
// it closes in the socket's own turn.
func TestSocketClosesAfterItsDependentsStop(t *testing.T) {
	for _, mode := range activationModes {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			up := filepath.Join(dir, "up")
			stopping := filepath.Join(dir, "stopping")
			release := filepath.Join(dir, "release")
			sock, svc := socketPair(t, mode, dir, "sock")
			svc.ExecStart = []string{"/bin/sleep", "60"}
			consumer := shellUnit("consumer.service", fmt.Sprintf(
				`trap 'touch %s; while [ ! -f %s ]; do sleep 0.02; done; exit 0' TERM; touch %s; sleep 60 & wait`,
				stopping, release, up), sock.Name)
			path := sock.ListenStream[0].Address

			d := pid1.NewDispatcher()
			dispStop := make(chan struct{})
			defer close(dispStop)
			d.Start(dispStop)
			sup, err := New([]*unit.Unit{sock, svc, consumer}, nil, d, &cgroup.Manager{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			go sup.Run()
			waitListening(t, sup, sock.Name)
			waitFile(t, up)

			sup.Stop()
			waitFile(t, stopping)
			time.Sleep(100 * time.Millisecond)
			_, statErr := os.Stat(path)
			_ = os.WriteFile(release, nil, 0o600)
			if statErr != nil {
				t.Errorf("socket closed while consumer.service was still stopping: %v", statErr)
			}
			select {
			case <-sup.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("supervisor did not shut down")
			}
			if _, err := os.Stat(path); err == nil {
				t.Error("socket still bound after shutdown")
			}
		})
	}
}

// TestOnFailureQueuedAcrossStop: a failure handler queued just before
// shutdown began is not invoked, even once shutdown has finished.
func TestOnFailureQueuedAcrossStop(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "fired")
	u := shellUnit("failing.service", "exit 1")
	u.OnFailure = []string{"handler.service"}
	handler := &unit.Unit{
		Name:      "handler.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{"/bin/touch", mark},
	}
	// Hold unitFailed at its log line, just before it queues the
	// handler, and shut down meanwhile.
	gate := newLogGate(t, "unit failing.service: failed:")
	sup := runUnits(t, u, handler)
	gate.wait(t)
	sup.Stop()
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
	gate.open()
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(gate.String(), "OnFailure: invoking handler.service") {
		t.Error("OnFailure= target invoked after shutdown")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("OnFailure= target ran after shutdown")
	}
}

// waitFile waits until f exists.
func waitFile(t *testing.T, f string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(f); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", f)
}
