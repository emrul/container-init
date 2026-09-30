//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// stateSupervisor is a supervisor writing the state file at path, not
// yet running. configure adjusts its writer before Run.
func stateSupervisor(t *testing.T, path string, configure func(*stateWriter), us ...*unit.Unit) *Supervisor {
	t.Helper()
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	d.Start(dispStop)
	t.Cleanup(func() { close(dispStop); <-d.Done() })
	sup, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	sup.SetStateFile(path, "vtest")
	if configure != nil {
		configure(sup.sw)
	}
	return sup
}

// run runs sup until the test ends, and returns a function that stops
// it and waits for Run to return.
func run(t *testing.T, sup *Supervisor) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { sup.Run(); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { sup.Stop(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// waitFile waits until the state file at path satisfies ok.
func waitStateFile(t *testing.T, path, what string, ok func(*statefile.File) bool) *statefile.File {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := statefile.Read(path)
		if err == nil && ok(f) {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("state file never %s (last read: %+v, %v)", what, f, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The file is there before the first unit starts, and names PID 1.
func TestStateFileWrittenBeforeFirstUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run", "state.json")
	out := filepath.Join(dir, "saw")
	u := shService("first.service", unit.TypeOneshot,
		fmt.Sprintf("if [ -f '%s' ]; then echo yes; else echo no; fi > '%s'", path, out))
	sup := stateSupervisor(t, path, nil, u)
	run(t, sup)
	f := waitStateFile(t, path, "showed first.service done", func(f *statefile.File) bool {
		return f.Units[u.Name].Runs == 1 && f.Units[u.Name].Active == "inactive"
	})
	if got, _ := os.ReadFile(out); strings.TrimSpace(string(got)) != "yes" {
		t.Errorf("state file present when the first unit ran: %q, want yes", got)
	}
	if f.PID1.Version != "vtest" || f.PID1.Stopping || !f.PID1.Started.Equal(sup.started.Truncate(time.Second)) {
		t.Errorf("pid1 = %+v", f.PID1)
	}
}

// A state change is written at once, without waiting for a heartbeat.
func TestStateFileWrittenOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	gate := filepath.Join(t.TempDir(), "go")
	u := shService("job.service", unit.TypeOneshot, fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
	sup := stateSupervisor(t, path, func(w *stateWriter) { w.tick = time.Hour }, u)
	run(t, sup)
	waitStateFile(t, path, "showed the job running", func(f *statefile.File) bool {
		return f.Units[u.Name].Active == "activating"
	})
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitStateFile(t, path, "showed the job done", func(f *statefile.File) bool {
		return f.Units[u.Name].Active == "inactive" && f.Units[u.Name].Runs == 1
	})
}

// The heartbeat advances `written` while nothing changes.
func TestStateFileHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	u := shService("idle.service", unit.TypeSimple, "exec sleep 60")
	sup := stateSupervisor(t, path, func(w *stateWriter) { w.tick = 100 * time.Millisecond }, u)
	run(t, sup)
	first := waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
	waitStateFile(t, path, "advanced written", func(f *statefile.File) bool {
		return f.Written.After(first.Written.Time)
	})
}

// With a heartbeat that never comes, changes are written with `written`
// carried forward. (One supervisor per test: each has its own
// dispatcher reaping any child, as PID 1 has only one.)
func TestStateFileChangeKeepsWritten(t *testing.T) {
	path2 := filepath.Join(t.TempDir(), "state.json")
	gate := filepath.Join(t.TempDir(), "go")
	job := shService("job.service", unit.TypeOneshot, fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
	sup2 := stateSupervisor(t, path2, func(w *stateWriter) { w.tick = time.Hour }, job)
	run(t, sup2)
	before := waitStateFile(t, path2, "showed the job", func(f *statefile.File) bool {
		return f.Units[job.Name].Runs == 1
	})
	time.Sleep(1100 * time.Millisecond) // a whole second later
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	after := waitStateFile(t, path2, "showed the job done", func(f *statefile.File) bool {
		return f.Units[job.Name].Active == "inactive"
	})
	if !after.Written.Equal(before.Written.Time) {
		t.Errorf("a change write moved written from %v to %v", before.Written, after.Written)
	}
}

// Reverse shutdown reports stopping at once, and its final write shows
// the units stopped.
func TestStateFileAtShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	u := shService("daemon.service", unit.TypeSimple, "exec sleep 60")
	sup := stateSupervisor(t, path, func(w *stateWriter) { w.tick = time.Hour }, u)
	stop := run(t, sup)
	waitStateFile(t, path, "showed the daemon running", func(f *statefile.File) bool {
		return f.Units[u.Name].Active == "active"
	})
	stop()
	f, err := statefile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.PID1.Stopping || f.Units[u.Name].Active != "inactive" || f.Units[u.Name].Sub != "dead" {
		t.Errorf("final file: stopping %v, daemon %+v; want stopping, inactive/dead", f.PID1.Stopping, f.Units[u.Name])
	}
}

// A writer stuck in I/O holds up nothing: the boot waits for the first
// write only briefly, units start and stop, and shutdown returns within
// its deadline.
func TestStateFileStalledWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	u := shService("job.service", unit.TypeOneshot, "exit 0")
	daemon := shService("daemon.service", unit.TypeSimple, "exec sleep 60")
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.write = func(string, *statefile.File) (statefile.Result, error) { <-stuck; return statefile.Result{}, nil }
	}, u, daemon)
	sup.SetStopTimeout(500 * time.Millisecond)
	start := time.Now()
	stop := run(t, sup)
	waitState(t, sup, u.Name, "finished", func(st statefile.Unit) bool { return st.Runs == 1 && st.Active == "inactive" })
	waitState(t, sup, daemon.Name, "running", is("active", "running"))
	if el := time.Since(start); el > firstWriteWait+2*time.Second {
		t.Errorf("units took %v to start behind a stalled writer", el)
	}
	stopStart := time.Now()
	stop()
	if el := time.Since(stopStart); el > 500*time.Millisecond+time.Second {
		t.Errorf("shutdown took %v behind a stalled writer (stop timeout 500ms)", el)
	}
}

// A failing write is logged once per failing stretch, and retried.
func TestStateFileWriteFailureLoggedOnce(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := filepath.Join(t.TempDir(), "state.json")
	var mu sync.Mutex
	fails := 5
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick = 20 * time.Millisecond
		w.write = func(p string, f *statefile.File) (statefile.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if fails > 0 {
				fails--
				return statefile.Result{}, errors.New("disk full")
			}
			return statefile.Write(p, f)
		}
	}, shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
	run(t, sup)
	waitStateFile(t, path, "written after the failures", func(*statefile.File) bool { return true })
	if n := strings.Count(logs.String(), "write failed"); n != 1 {
		t.Errorf("logged %d write failures, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "writing again"); n != 1 {
		t.Errorf("logged %d recoveries, want 1", n)
	}
}

// As root, a state file directory another uid can write is warned
// about once; a sticky one is not.
func TestStateFileWritableDirectoryWarning(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o777, true}, {0o777 | os.ModeSticky, false}, {0o755, false}} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			var logs syncBuffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			dir := filepath.Join(t.TempDir(), "d")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.json")
			sup := stateSupervisor(t, path, func(w *stateWriter) { w.tick = 20 * time.Millisecond },
				shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
			run(t, sup)
			waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
			time.Sleep(100 * time.Millisecond) // several heartbeats
			if n := strings.Count(logs.String(), "can replace the state file"); (n == 1) != tc.warn || n > 1 {
				t.Errorf("warned %d time(s), want warning %v", n, tc.warn)
			}
		})
	}
}

// Without SetStateFile nothing is written, and no writer runs.
func TestStateFileOff(t *testing.T) {
	sup := stateSupervisor(t, "", nil, shService("x.service", unit.TypeOneshot, "exit 0"))
	if sup.sw != nil {
		t.Fatal("a state writer exists with no path")
	}
	run(t, sup)
	waitState(t, sup, "x.service", "finished", func(st statefile.Unit) bool { return st.Runs == 1 })
}

// stuckReaper stands in for a dispatcher loop that has stopped: it
// takes one ping and never answers. While answering is true it answers
// at once instead.
type stuckReaper struct {
	mu        sync.Mutex
	answering bool
	pending   chan uint64
	pongs     chan uint64
	refused   int
}

func newStuckReaper(answering bool) *stuckReaper {
	return &stuckReaper{answering: answering, pending: make(chan uint64, 1), pongs: make(chan uint64, 1)}
}

func (r *stuckReaper) Ping(seq uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answering {
		select {
		case r.pongs <- seq:
		default:
		}
		return true
	}
	select {
	case r.pending <- seq:
		return true
	default:
		r.refused++
		return false
	}
}

func (r *stuckReaper) Pongs() <-chan uint64 { return r.pongs }

func (r *stuckReaper) stop() {
	r.mu.Lock()
	r.answering = false
	r.mu.Unlock()
}

// With the reaper stuck from startup, the first file's written is PID
// 1's start and never advances, while state changes are still written.
func TestHeartbeatReaperStuckFromStart(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := filepath.Join(t.TempDir(), "state.json")
	gate := filepath.Join(t.TempDir(), "go")
	job := shService("job.service", unit.TypeOneshot, fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick, w.budget = 50*time.Millisecond, 50*time.Millisecond
	}, job)
	reaper := newStuckReaper(false)
	sup.live = reaper
	run(t, sup)
	first := waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
	if !first.Written.Equal(first.PID1.Started.Time) {
		t.Errorf("first written %v, want pid1.started %v", first.Written, first.PID1.Started)
	}
	time.Sleep(1200 * time.Millisecond) // past a whole second, many heartbeats
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := waitStateFile(t, path, "showed the job done", func(f *statefile.File) bool {
		return f.Units[job.Name].Active == "inactive" && f.Units[job.Name].Runs == 1
	})
	if !f.Written.Equal(first.Written.Time) {
		t.Errorf("written moved from %v to %v with the reaper stuck", first.Written, f.Written)
	}
	if n := strings.Count(logs.String(), "reaper did not answer"); n != 1 {
		t.Errorf("logged the reaper check %d times, want once per failing stretch", n)
	}
}

// A reaper that stops answering mid-run stops the heartbeat; however
// many heartbeats pass, at most one ping is outstanding and no
// goroutines pile up.
func TestHeartbeatReaperStuckLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick, w.budget = 20*time.Millisecond, 20*time.Millisecond
	}, shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
	reaper := newStuckReaper(true)
	sup.live = reaper
	run(t, sup)
	first := waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
	waitStateFile(t, path, "advanced written", func(f *statefile.File) bool { return f.Written.After(first.Written.Time) })
	reaper.stop()
	time.Sleep(100 * time.Millisecond) // let a heartbeat in flight finish
	stuck, _ := statefile.Read(path)
	goroutines := runtime.NumGoroutine()
	time.Sleep(1200 * time.Millisecond) // ~60 heartbeats
	now, _ := statefile.Read(path)
	if !now.Written.Equal(stuck.Written.Time) {
		t.Errorf("written advanced from %v to %v with the reaper stuck", stuck.Written, now.Written)
	}
	if len(reaper.pending) > 1 {
		t.Errorf("%d pings outstanding", len(reaper.pending))
	}
	reaper.mu.Lock()
	refused := reaper.refused
	reaper.mu.Unlock()
	if refused < 10 {
		t.Errorf("only %d heartbeats tried to ping a stuck reaper", refused)
	}
	if g := runtime.NumGoroutine(); g > goroutines+2 {
		t.Errorf("goroutines grew from %d to %d while the reaper was stuck", goroutines, g)
	}
}

// The state lock held (a deadlocked supervisor) stops the heartbeat,
// and it resumes once the lock is free.
func TestHeartbeatStateLockHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick, w.budget = 50*time.Millisecond, 100*time.Millisecond
	}, shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
	run(t, sup)
	waitState(t, sup, "daemon.service", "running", is("active", "running"))
	first := waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
	waitStateFile(t, path, "advanced written", func(f *statefile.File) bool { return f.Written.After(first.Written.Time) })
	sup.mu.Lock()
	held, _ := statefile.Read(path)
	time.Sleep(1200 * time.Millisecond)
	during, _ := statefile.Read(path)
	sup.mu.Unlock()
	if !during.Written.Equal(held.Written.Time) {
		t.Errorf("written advanced from %v to %v with the state lock held", held.Written, during.Written)
	}
	waitStateFile(t, path, "advanced written again", func(f *statefile.File) bool { return f.Written.After(during.Written.Time) })
}

// A unit recorded running whose process is gone -- its exit reaped but
// never handled -- fails the heartbeat from the second sighting on.
func TestHeartbeatUnhandledExit(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := filepath.Join(t.TempDir(), "state.json")
	ghost := shService("ghost.service", unit.TypeSimple, "exit 0")
	ghost.Condition.Skip, ghost.Condition.Reason = true, "held by the test"
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick = 50 * time.Millisecond
	}, shService("daemon.service", unit.TypeSimple, "exec sleep 60"), ghost)
	run(t, sup)
	first := waitStateFile(t, path, "written", func(*statefile.File) bool { return true })
	waitStateFile(t, path, "advanced written", func(f *statefile.File) bool { return f.Written.After(first.Written.Time) })

	// A pid that has exited and been reaped, held as ghost.service's
	// running process. The dispatcher reaps it: it reaps every child.
	cmd := exec.Command("/bin/true")
	pid, exited, err := sup.dispatcher.Spawn(cmd)
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Release()
	<-exited
	sup.mu.Lock()
	sup.services[ghost.Name] = &serviceState{name: ghost.Name, pid: pid}
	sup.status[ghost.Name].Active, sup.status[ghost.Name].Sub = "active", "running"
	sup.mu.Unlock()
	time.Sleep(200 * time.Millisecond) // two heartbeats or more
	stuck, _ := statefile.Read(path)
	time.Sleep(1200 * time.Millisecond)
	now, _ := statefile.Read(path)
	if !now.Written.Equal(stuck.Written.Time) {
		t.Errorf("written advanced from %v to %v with an unhandled exit", stuck.Written, now.Written)
	}
	if !strings.Contains(logs.String(), "exit not handled for ghost.service") {
		t.Errorf("no log naming the unhandled exit:\n%s", logs.String())
	}
	// Seeing the exit is not enough while the unit is still reported
	// running; handling it -- the state moving on -- resumes the
	// heartbeat.
	sup.mu.Lock()
	sup.services[ghost.Name].exited = true
	sup.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	if f, _ := statefile.Read(path); !f.Written.Equal(now.Written.Time) {
		t.Errorf("written advanced to %v with the exit seen but the unit still running", f.Written)
	}
	sup.setState(ghost.Name, "inactive", "dead")
	waitStateFile(t, path, "advanced written again", func(f *statefile.File) bool { return f.Written.After(now.Written.Time) })
}
