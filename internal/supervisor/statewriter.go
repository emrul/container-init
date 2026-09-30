package supervisor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emrul/container-init/internal/statefile"
)

// The state writer: one goroutine owns the --state-file, writing it at
// startup, on every state change (coalesced: one pending signal covers
// any number) and on the heartbeat. Nothing in the lifecycle waits for
// it, except startup and reverse shutdown, each for a bounded time: a
// writer stuck in I/O costs a stale file, never a stuck unit.

const (
	// heartbeatInterval is how often the heartbeat advances `written`.
	heartbeatInterval = 10 * time.Second
	// checkBudget bounds each heartbeat check, and taking the state
	// snapshot for any write.
	checkBudget = 2 * time.Second
	// firstWriteWait bounds how long Run waits for the first write
	// before starting units.
	firstWriteWait = checkBudget
)

type stateWriter struct {
	path, version string
	tick          time.Duration
	budget        time.Duration // checkBudget; a field for tests
	// write replaces the file; a field so tests can stall it.
	write func(path string, f *statefile.File) (statefile.Result, error)

	mu        sync.Mutex
	seq       uint64        // writes begun
	completed uint64        // the seq of the last write finished
	wrote     chan struct{} // closed, and replaced, when a write finishes
	quit      chan struct{} // closed by shutdown, after the final write
	exited    chan struct{} // closed when the writer goroutine returns

	// Owned by the writer goroutine.
	written  time.Time
	last     map[string]statefile.Unit
	failing  bool
	dirNoted bool
	pingSeq  uint64
	// suspect holds the pids that looked exited-but-unhandled on the
	// last heartbeat; a second sighting fails the check.
	suspect map[int]bool
	// checkFailed is the heartbeat check failing now, "" when all pass.
	checkFailed string
}

// liveness is the reaper's side of heartbeat check 2: pid1.Dispatcher,
// or a stand-in in tests.
type liveness interface {
	Ping(seq uint64) bool
	Pongs() <-chan uint64
}

// SetStateFile makes Run write every unit's state to path (see
// docs/design/state-file.md), reporting version as PID 1's. An empty
// path, the default, writes nothing. Call before Run.
func (s *Supervisor) SetStateFile(path, version string) {
	if path == "" {
		s.sw = nil
		return
	}
	s.sw = &stateWriter{
		path:    path,
		version: version,
		tick:    heartbeatInterval,
		budget:  checkBudget,
		write:   statefile.Write,
		wrote:   make(chan struct{}),
		quit:    make(chan struct{}),
		exited:  make(chan struct{}),
		written: s.started,
	}
}

// startStateWriter starts the writer and waits, for at most
// firstWriteWait, for its first write: the file should exist before the
// first unit starts, but a hung filesystem must not hold up the boot.
func (s *Supervisor) startStateWriter() {
	if s.sw == nil {
		return
	}
	go s.runStateWriter()
	if !s.waitStateWrite(0, time.Now().Add(firstWriteWait)) {
		log.Printf("state file %s: first write not done within %v; starting units anyway", s.sw.path, firstWriteWait)
	}
}

func (s *Supervisor) runStateWriter() {
	w := s.sw
	defer close(w.exited)
	s.writeState(true)
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.changed:
			s.writeState(false)
		case <-ticker.C:
			s.writeState(true)
		case <-w.quit:
			return
		}
	}
}

// writeState writes the file once. beat says the heartbeat asked: only
// then does `written` advance; a write for a state change carries it
// forward.
func (s *Supervisor) writeState(beat bool) {
	w := s.sw
	w.mu.Lock()
	w.seq++
	seq := w.seq
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.completed = seq
		close(w.wrote)
		w.wrote = make(chan struct{})
		w.mu.Unlock()
	}()

	now := time.Now()
	deadline := now.Add(w.budget)
	units, running, ok := s.snapshotState(deadline)
	if ok {
		w.last = units
	}
	if beat {
		s.heartbeat(now, deadline, ok, running)
	}
	if w.last == nil {
		return // nothing to report yet
	}
	f := &statefile.File{
		Version: statefile.Version,
		Written: statefile.Time{Time: w.written},
		PID1: statefile.PID1{
			Version:  w.version,
			Started:  statefile.Time{Time: s.started},
			Stopping: s.stopping(),
		},
		Units: w.last,
	}
	res, err := w.write(w.path, f)
	for _, d := range res.Created {
		log.Printf("state file: created directory %s", d)
	}
	for _, d := range res.Unwidened {
		log.Printf("state file: warning: created %s beneath a directory another uid can write; "+
			"left at mkdir's mode rather than set to 0755, since that uid could have replaced it", d)
	}
	if err != nil {
		if !w.failing {
			log.Printf("state file %s: write failed: %v (retrying on the next change or heartbeat)", w.path, err)
			w.failing = true
		}
		return
	}
	if w.failing {
		log.Printf("state file %s: writing again", w.path)
		w.failing = false
	}
	if !w.dirNoted {
		w.dirNoted = true
		s.noteStateDir(filepath.Dir(w.path))
	}
}

// snapshotState copies every unit's state, and the pid of each unit
// recorded running (or, for a oneshot, start) whose exit has not been
// seen. It takes the state lock with TryLock, retried until deadline,
// from the writer goroutine itself: a supervisor stuck holding it
// leaves nothing behind waiting on it.
func (s *Supervisor) snapshotState(deadline time.Time) (map[string]statefile.Unit, map[string]int, bool) {
	for {
		if s.mu.TryLock() {
			units := s.stateLocked()
			running := map[string]int{}
			for name, st := range units {
				live := st.Active == statefile.ActiveActive && st.Sub == statefile.SubRunning ||
					st.Active == statefile.ActiveActivating && st.Sub == statefile.SubStart
				if p := s.services[name]; live && p != nil && !p.exited && p.pid > 0 {
					running[name] = p.pid
				}
			}
			s.mu.Unlock()
			return units, running, true
		}
		if time.Now().After(deadline) {
			return nil, nil, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// heartbeat advances `written` to now if the supervisor passes every
// check within the budget ending at deadline: the state lock was taken
// (locked), the reaper answers, and every exit has been noticed.
// Nothing it does can pile up while the supervisor stays stuck: the
// lock was only tried, and at most one ping is ever outstanding.
func (s *Supervisor) heartbeat(now, deadline time.Time, locked bool, running map[string]int) {
	w := s.sw
	failed := ""
	switch {
	case !locked:
		failed = "the state lock was not free within " + w.budget.String()
	case !s.pingReaper(deadline):
		failed = "the reaper did not answer within " + w.budget.String()
	default:
		failed = w.checkExits(running)
	}
	if failed == "" {
		w.written = now
		if w.checkFailed != "" {
			log.Printf("state file: heartbeat checks pass again")
		}
	} else if failed != w.checkFailed {
		log.Printf("state file: heartbeat check failed: %s; not advancing written", failed)
	}
	w.checkFailed = failed
}

// pingReaper sends one ping through the dispatcher's loop and waits
// until deadline for its answer. A ping still unanswered from an
// earlier heartbeat makes the send fail, and this heartbeat with it.
func (s *Supervisor) pingReaper(deadline time.Time) bool {
	w := s.sw
	pongs := s.live.Pongs()
	for drained := false; !drained; {
		select {
		case <-pongs: // a late answer to an earlier ping
		default:
			drained = true
		}
	}
	w.pingSeq++
	seq := w.pingSeq
	if !s.live.Ping(seq) {
		return false
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case got := <-pongs:
			if got == seq {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

// checkExits fails when a unit's recorded process is a zombie, or gone
// while the unit is still recorded running, on two heartbeats in a
// row: its exit was not reaped, or not handled. One sighting can be the
// normal moment between an exit and its handling.
func (w *stateWriter) checkExits(running map[string]int) string {
	suspect := map[int]bool{}
	var stuck []string
	for name, pid := range running {
		if processRunning(pid) {
			continue
		}
		suspect[pid] = true
		if w.suspect[pid] {
			stuck = append(stuck, fmt.Sprintf("%s (pid %d)", name, pid))
		}
	}
	w.suspect = suspect
	if len(stuck) == 0 {
		return ""
	}
	sort.Strings(stuck)
	return "exit not handled for " + strings.Join(stuck, ", ")
}

// waitStateWrite waits until a write begun after the writer's seq-th
// has finished, or deadline passes. Reports whether it finished.
func (s *Supervisor) waitStateWrite(seq uint64, deadline time.Time) bool {
	w := s.sw
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		w.mu.Lock()
		done, wrote := w.completed > seq, w.wrote
		w.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-wrote:
		case <-timer.C:
			return false
		}
	}
}

// closeState writes the state as it is now, then stops the writer,
// waiting for both until deadline -- reverse shutdown's own, so the
// final report adds no time to shutdown. A writer still busy then is
// left to the process's exit.
func (s *Supervisor) closeState(deadline time.Time) {
	w := s.sw
	if w == nil {
		return
	}
	w.mu.Lock()
	seq := w.seq
	w.mu.Unlock()
	s.stateChanged()
	if !s.waitStateWrite(seq, deadline) {
		log.Printf("state file %s: final write not done by the shutdown deadline", w.path)
	}
	close(w.quit)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-w.exited:
	case <-timer.C:
	}
}

// noteStateDir warns, once, when PID 1 is root and the state file's
// directory is writable by another uid: that user can replace the file
// and forge the report. Writing stays safe; only the contents cannot
// be trusted. A sticky directory is exempt: nobody else can replace
// root's file in it.
func (s *Supervisor) noteStateDir(dir string) {
	if s.euid != 0 {
		return
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	othersWrite := fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0
	if st.Uid != 0 || othersWrite {
		log.Printf("state file: warning: directory %s is writable by a uid other than root (owner %d, mode %04o); "+
			"that user can replace the state file and forge the report", dir, st.Uid, fi.Mode().Perm())
	}
}
