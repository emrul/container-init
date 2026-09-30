//go:build unix

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/health"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// An answering dispatcher isolates the state/pid check without starting
// another process-wide child reaper.
type answeringReaper struct{ replies chan uint64 }

func (r *answeringReaper) Ping(seq uint64) bool { r.replies <- seq; return true }
func (r *answeringReaper) Pongs() <-chan uint64 { return r.replies }

func unstartedStateSupervisor(t *testing.T, u *unit.Unit) *Supervisor {
	t.Helper()
	s, err := New([]*unit.Unit{u}, nil, pid1.NewDispatcher(), &cgroup.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStateFile(filepath.Join(t.TempDir(), "state.json"), "vtest")
	s.live = &answeringReaper{make(chan uint64, 1)}
	return s
}

func TestHeartbeatMustNotCertifyExitedRunningUnit(t *testing.T) {
	u := &unit.Unit{Name: "dead.service", Kind: unit.KindService, Type: unit.TypeSimple}
	s := unstartedStateSupervisor(t, u)
	// This is finish's intermediate state before orphan cleanup returns
	// and the lifecycle loop changes active/sub. Use a nonexistent PID;
	// run 1 is the run spawnAndWait records for the unit's first start.
	s.services[u.Name] = &serviceState{name: u.Name, pid: 1 << 30, exited: true, run: 1}
	s.update(u.Name, func(st *statefile.Unit) {
		st.Active, st.Sub, st.Runs = "active", "running", 1
	})
	s.writeState(true)
	first := s.sw.written
	s.writeState(true)
	if s.sw.written.After(first) {
		t.Error("heartbeat advanced twice while a unit still reports active/running with an exited process")
	}
	f, err := statefile.Read(s.sw.path)
	if err != nil {
		t.Fatal(err)
	}
	// Two heartbeats in, written is frozen at the first; once it ages
	// past --max-age, health fails the container.
	if ok, why := health.Check(f, s.sw.written.Add(health.DefaultMaxAge+time.Second), []string{u.Name}, health.DefaultMaxAge); ok {
		t.Errorf("health passes a dead service once the heartbeat is stale: %s", why)
	}
}

func TestFinalStateStopsStartBlockedBeforeSpawn(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "environment")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Holding an RDWR endpoint makes the read block until we close it.
	hold, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := &unit.Unit{Name: "blocked.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/true"}, EnvironmentFile: []unit.EnvFileRef{{Path: fifo}}}
	s := unstartedStateSupervisor(t, u)
	s.SetStopTimeout(time.Second)
	s.startStateWriter()
	done := make(chan struct{})
	go func() { s.runService(u); close(done) }()
	t.Cleanup(func() {
		hold.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("blocked start did not exit after releasing its environment file")
		}
	})
	deadline := time.Now().Add(time.Second)
	for s.State()[u.Name].Runs == 0 {
		if time.Now().After(deadline) {
			t.Fatal("start not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	// Give the reader time to open the FIFO before cleanup closes it.
	time.Sleep(20 * time.Millisecond)
	s.Stop()
	s.shutdown()
	f, err := statefile.Read(s.sw.path)
	if err != nil {
		t.Fatal(err)
	}
	if st := f.Units[u.Name]; st.Active != "inactive" || st.Sub != "dead" {
		t.Errorf("final shutdown file still says %s/%s for a start that can no longer spawn", st.Active, st.Sub)
	}
}

// A restart admitted but not yet spawned (blocked before its exec, say
// reading an EnvironmentFile=) still has the previous run's exited
// process on record: that is not an unhandled exit.
func TestHeartbeatIgnoresPreviousRunOfPendingStart(t *testing.T) {
	u := &unit.Unit{Name: "restarting.service", Kind: unit.KindService, Type: unit.TypeSimple}
	s := unstartedStateSupervisor(t, u)
	s.services[u.Name] = &serviceState{name: u.Name, pid: 1 << 30, exited: true, run: 1}
	s.update(u.Name, func(st *statefile.Unit) {
		st.Active, st.Sub, st.Runs, st.Restarts = "activating", "start", 2, 1
	})
	s.writeState(true)
	first := s.sw.written
	time.Sleep(1100 * time.Millisecond) // written has whole-second resolution in the file, not here
	s.writeState(true)
	s.writeState(true)
	if !s.sw.written.After(first) || s.sw.checkFailed != "" {
		t.Errorf("heartbeat failed (%q) on a pending start's previous run", s.sw.checkFailed)
	}
}
