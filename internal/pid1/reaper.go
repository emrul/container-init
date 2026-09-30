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
// descriptive error otherwise.
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
// reaped exit statuses to per-pid channels. It is both the only reaper
// and the only fork-exec entry point (Spawn): a second wait4(-1) or a
// cmd.Wait elsewhere would race it for statuses. Orphaned grandchildren
// (double-forking daemons, Type=forking services) are reaped too, and
// dropped.
type Dispatcher struct {
	mu      sync.Mutex
	pending map[int]chan ExitStatus

	startOnce sync.Once
	notify    chan os.Signal
	stop      <-chan struct{}
	done      chan struct{}
	// ping and pong carry liveness checks through the loop; see Ping.
	ping, pong chan uint64
}

// NewDispatcher constructs an unstarted dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		pending: make(map[int]chan ExitStatus),
		done:    make(chan struct{}),
		ping:    make(chan uint64, 1),
		pong:    make(chan uint64, 1),
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

// Spawn fork-execs cmd under the dispatcher lock, so the child's
// channel is registered before any drain can reap it. Callers must
// start children only through Spawn: with cmd.Start, a child that
// exits at once can be reaped before its channel exists, and its
// status is lost.
//
// After Spawn returns, the caller releases cmd.Process (Go's wait
// machinery is unused) and signals the child with syscall.Kill on the
// returned pid, not cmd.Process.Kill, which fails after Release.
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

// Track registers a pid the caller did not start with Spawn and
// returns the channel that receives its reap status. The caller must
// guarantee the pid has not been reaped since it was created.
func (d *Dispatcher) Track(pid int) <-chan ExitStatus {
	ch := make(chan ExitStatus, 1)
	d.mu.Lock()
	d.pending[pid] = ch
	d.mu.Unlock()
	return ch
}

// Untrack drops pid's registration; its exit is then reaped and
// dropped.
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
			// A final sweep delivers any exit that arrived since the
			// last drain.
			d.drain()
			return
		case <-d.notify:
			// Coalesce: drain reaps everything currently waitable.
		case seq := <-d.ping:
			// Answer only after a drain of our own: it takes the
			// lock, so a wedged drain or a stuck Spawn holding it
			// keeps the answer from coming.
			d.drain()
			select {
			case d.pong <- seq:
			default: // an unread answer is still there
			}
		}
	}
}

// Ping asks the loop to show it is live: it answers seq on Pongs once
// it has drained, between drains, as it does its real work. It never
// blocks: it reports false, queueing nothing, while an earlier ping is
// still unanswered, so however long the loop is stuck at most one ping
// is outstanding.
func (d *Dispatcher) Ping(seq uint64) bool {
	select {
	case d.ping <- seq:
		return true
	default:
		return false
	}
}

// Pongs is where the loop answers pings, with their seq. A reader
// should discard a seq older than the one it waits for.
func (d *Dispatcher) Pongs() <-chan uint64 { return d.pong }

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
			// An orphan reparented onto PID 1: reaped and dropped.
			continue
		}
		delete(d.pending, pid)
		es := buildExitStatus(pid, ws)
		select {
		case ch <- es:
		default:
			// Buffered to 1: only a pid registered twice fills it.
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

// ForwardSignals calls onShutdown once, with the first SIGTERM or
// SIGINT received, and returns; or returns when stop is closed.
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
