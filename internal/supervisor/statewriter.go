package supervisor

import (
	"log"
	"os"
	"path/filepath"
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
	// write replaces the file; a field so tests can stall it.
	write func(path string, f *statefile.File) ([]string, error)

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
	units, ok := s.snapshotState(now.Add(checkBudget))
	if ok {
		w.last = units
	}
	if beat && ok {
		w.written = now
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
	created, err := w.write(w.path, f)
	for _, d := range created {
		log.Printf("state file: created directory %s", d)
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

// snapshotState copies every unit's state, taking the state lock with
// TryLock, retried until deadline, from the writer goroutine itself:
// a supervisor stuck holding it leaves nothing behind waiting on it.
func (s *Supervisor) snapshotState(deadline time.Time) (map[string]statefile.Unit, bool) {
	for {
		if s.mu.TryLock() {
			units := s.stateLocked()
			s.mu.Unlock()
			return units, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(50 * time.Millisecond)
	}
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
