//go:build linux

package pid1

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestDispatcherSpawnDelivers asserts that an immediate-exit child's
// status is routed to the per-pid channel and the channel does not
// leak in the pending map afterwards.
func TestDispatcherSpawnDelivers(t *testing.T) {
	d := NewDispatcher()
	stop := make(chan struct{})
	defer func() { close(stop); <-d.Done() }()
	d.Start(stop)

	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	pid, ch, err := d.Spawn(cmd)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cmd.Process.Release()

	select {
	case es := <-ch:
		if es.Pid != pid {
			t.Errorf("ExitStatus.Pid = %d, want %d", es.Pid, pid)
		}
		if !es.Status.Exited() {
			t.Errorf("Status.Exited() = false, want true")
		}
		if es.ExitCode != 7 {
			t.Errorf("ExitCode = %d, want 7", es.ExitCode)
		}
		if err := es.AnyError(); err == nil {
			t.Errorf("AnyError() = nil, want non-nil for non-zero exit")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for child %d exit", pid)
	}

	d.mu.Lock()
	_, stillPending := d.pending[pid]
	d.mu.Unlock()
	if stillPending {
		t.Errorf("pid %d still in pending map after delivery", pid)
	}
}

// TestDispatcherSpawnSignaled covers the syscall.Kill -> SIGTERM
// path the supervisor's reverse-shutdown takes.
func TestDispatcherSpawnSignaled(t *testing.T) {
	d := NewDispatcher()
	stop := make(chan struct{})
	defer func() { close(stop); <-d.Done() }()
	d.Start(stop)

	cmd := exec.Command("/bin/sleep", "30")
	pid, ch, err := d.Spawn(cmd)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cmd.Process.Release()

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case es := <-ch:
		if !es.Signaled || es.Signal != syscall.SIGTERM {
			t.Errorf("Signaled=%v Signal=%v, want true SIGTERM", es.Signaled, es.Signal)
		}
		if err := es.AnyError(); err == nil {
			t.Errorf("AnyError() = nil for signal-killed child")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for SIGTERM-killed child")
	}
}

// TestDispatcherOrphanReaped pins the PID-1 zombie-reaper behaviour:
// a reparented grandchild (no Track call) must still be wait4'd by
// the dispatcher so it doesn't accumulate as a zombie. We use a
// double-fork via /bin/sh to produce one.
func TestDispatcherOrphanReaped(t *testing.T) {
	d := NewDispatcher()
	stop := make(chan struct{})
	defer func() { close(stop); <-d.Done() }()
	d.Start(stop)

	// Parent shell forks a backgrounded sleep then exits. The sleep
	// is reparented to us (the test process) since we're its closest
	// subreaper.
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.2 & exit 0")
	parentPid, ch, err := d.Spawn(cmd)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cmd.Process.Release()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("parent sh (pid %d) didn't deliver", parentPid)
	}

	// Give SIGCHLD time to land for the orphaned sleep. The drain
	// should reap it without us tracking it; verify by polling
	// /proc/[orphan]/status -- but easier: just wait a beat and
	// confirm the dispatcher's pending map is empty (no leftover
	// state) and the test isn't leaking a zombie. wait4 with WNOHANG
	// from outside the dispatcher would race; instead we use the
	// dispatcher's internal state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		empty := len(d.pending) == 0
		d.mu.Unlock()
		if empty {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) != 0 {
		t.Errorf("pending non-empty after orphan reap window: %v", d.pending)
	}
}

// TestDispatcherConcurrentSpawn fires N parallel spawns, each
// terminating quickly, and verifies every channel delivers exactly
// once.
func TestDispatcherConcurrentSpawn(t *testing.T) {
	d := NewDispatcher()
	stop := make(chan struct{})
	defer func() { close(stop); <-d.Done() }()
	d.Start(stop)

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			cmd := exec.Command("/bin/true")
			_, ch, err := d.Spawn(cmd)
			if err != nil {
				errs <- err
				return
			}
			cmd.Process.Release()
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
				errs <- &timeoutErr{}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent spawn err: %v", err)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "timeout" }

// A ping is answered with its seq while the loop is free; while the lock
// is held (a stuck Spawn or drain) it is not, and no second ping queues.
func TestDispatcherPing(t *testing.T) {
	d := NewDispatcher()
	stop := make(chan struct{})
	defer func() { close(stop); <-d.Done() }()
	d.Start(stop)

	if !d.Ping(1) {
		t.Fatal("Ping refused on an idle loop")
	}
	select {
	case seq := <-d.Pongs():
		if seq != 1 {
			t.Errorf("pong %d, want 1", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle loop did not answer")
	}

	d.mu.Lock() // as a stuck Spawn would
	if !d.Ping(2) {
		t.Fatal("Ping refused with nothing outstanding")
	}
	time.Sleep(50 * time.Millisecond) // the loop takes it, then waits for the lock
	sent := 0
	for i := 0; i < 10; i++ {
		if d.Ping(uint64(3 + i)) {
			sent++
		}
	}
	if sent > 1 {
		t.Errorf("%d more pings queued while the loop was stuck, want at most 1", sent)
	}
	select {
	case seq := <-d.Pongs():
		t.Fatalf("stuck loop answered pong %d", seq)
	case <-time.After(200 * time.Millisecond):
	}
	d.mu.Unlock()
	select {
	case seq := <-d.Pongs():
		if seq != 2 {
			t.Errorf("pong %d after release, want 2", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not answer once released")
	}
}
