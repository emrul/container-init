package pid1

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// ExitStatus carries the result of one wait4 reap, plus convenience
// fields decoded from WaitStatus so callers don't have to reach into
// the syscall package.
type ExitStatus struct {
	Pid      int
	Status   syscall.WaitStatus
	ExitCode int            // populated when Status.Exited()
	Signaled bool           // true when killed by signal
	Signal   syscall.Signal // populated when Signaled
}

// AnyError returns nil for a clean (exit 0) termination and a
// descriptive error otherwise. Maps directly onto the supervisor's
// "failed?" check.
func (es ExitStatus) AnyError() error {
	if es.Signaled {
		return fmt.Errorf("killed by %v", es.Signal)
	}
	if es.ExitCode != 0 {
		return fmt.Errorf("exit code %d", es.ExitCode)
	}
	return nil
}

// Dispatcher owns SIGCHLD handling for container-init and routes
// reaped exit statuses to per-pid channels. It replaces the original
// cmd.Wait()-driven reaping (design note #1): a standalone
// wait4(-1, …) reaper racing os/exec.Cmd.Wait silently steals child
// statuses and breaks Restart=/OnFailure=. By making the dispatcher
// the single source of truth for child reaping AND the supervisor's
// fork-exec entry point (Spawn), we close that race AND collect any
// orphaned grandchildren (dbus-launch double-forks, Type=forking
// services) without affecting their tracked-child counterpart.
type Dispatcher struct {
	mu      sync.Mutex
	pending map[int]chan ExitStatus

	startOnce sync.Once
	notify    chan os.Signal
	stop      <-chan struct{}
	done      chan struct{}
}

// NewDispatcher constructs an unstarted dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		pending: make(map[int]chan ExitStatus),
		done:    make(chan struct{}),
	}
}

// Start installs the SIGCHLD handler and begins draining. Idempotent.
// The dispatcher exits when stop is closed; Done() blocks until then.
func (d *Dispatcher) Start(stop <-chan struct{}) {
	d.startOnce.Do(func() {
		d.stop = stop
		d.notify = make(chan os.Signal, 16)
		signal.Notify(d.notify, syscall.SIGCHLD)
		go d.loop()
	})
}

// Done returns a channel closed when the dispatcher's loop exits.
func (d *Dispatcher) Done() <-chan struct{} { return d.done }

// Spawn fork-execs cmd under the dispatcher lock so that SIGCHLD
// delivery cannot race the pid-to-channel registration. Callers MUST
// use this entry point rather than cmd.Start directly -- without
// atomic registration, a fast-exiting child can deliver SIGCHLD
// before the supervisor has filed its consumer channel, and the exit
// status is silently dropped.
//
// After this returns, the os/exec.Process can be Released to free
// Go's pidfd handle (Go's wait machinery is unused; we own the PID).
// Callers signal the child via syscall.Kill on the returned PID, not
// cmd.Process.Kill (which fails after Release sets Pid=-1).
func (d *Dispatcher) Spawn(cmd *exec.Cmd) (int, <-chan ExitStatus, error) {
	ch := make(chan ExitStatus, 1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	d.pending[pid] = ch
	return pid, ch, nil
}

// Track is the lower-level entry point that registers a pid the
// caller has already created (e.g. a grandchild discovered via
// PIDFile=). The same race-window caveat applies as for Spawn -- the
// caller must guarantee the PID hasn't been waited on by anyone else
// since fork. Returns the channel that will receive the eventual
// reap status.
func (d *Dispatcher) Track(pid int) <-chan ExitStatus {
	ch := make(chan ExitStatus, 1)
	d.mu.Lock()
	d.pending[pid] = ch
	d.mu.Unlock()
	return ch
}

// Untrack drops a registration without delivering. Used when the
// supervisor decides a unit is being torn down and no longer cares
// about the eventual exit.
func (d *Dispatcher) Untrack(pid int) {
	d.mu.Lock()
	delete(d.pending, pid)
	d.mu.Unlock()
}

func (d *Dispatcher) loop() {
	defer signal.Stop(d.notify)
	defer close(d.done)
	for {
		d.drain()
		select {
		case <-d.stop:
			// Final sweep so any zombie reaped after the loop body
			// began still gets delivered.
			d.drain()
			return
		case <-d.notify:
			// Coalesce: drain reaps everything currently waitable.
		}
	}
}

func (d *Dispatcher) drain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err != nil {
			if err == syscall.ECHILD || err == syscall.EINTR {
				return
			}
			log.Printf("pid1/dispatcher: wait4: %v", err)
			return
		}
		if pid <= 0 {
			return
		}
		ch, ok := d.pending[pid]
		if !ok {
			// Orphan -- re-parented grandchild, silently reaped. This
			// is the path that catches dbus-launch's daemon child
			// after the launcher exits.
			continue
		}
		delete(d.pending, pid)
		es := buildExitStatus(pid, ws)
		select {
		case ch <- es:
		default:
			// Buffered to 1; the only way this fills is a programming
			// error (double-track of the same pid). Log loudly.
			log.Printf("pid1/dispatcher: dropped status for pid %d (channel full)", pid)
		}
	}
}

func buildExitStatus(pid int, ws syscall.WaitStatus) ExitStatus {
	es := ExitStatus{Pid: pid, Status: ws}
	if ws.Exited() {
		es.ExitCode = ws.ExitStatus()
	}
	if ws.Signaled() {
		es.Signaled = true
		es.Signal = ws.Signal()
	}
	return es
}

// ForwardSignals runs until stop is closed, forwarding SIGTERM and
// SIGINT to onShutdown. The supervisor is responsible for the actual
// reverse-shutdown sequence; this routine just turns external signals
// into a one-shot trigger.
func ForwardSignals(onShutdown func(os.Signal), stop <-chan struct{}) {
	notify := make(chan os.Signal, 4)
	signal.Notify(notify, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(notify)
	for {
		select {
		case <-stop:
			return
		case s := <-notify:
			onShutdown(s)
			return
		}
	}
}
