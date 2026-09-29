//go:build linux

package supervisor

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/unit"
)

// auditSupervisor provides the real dispatcher without eager boot activation.
func auditSupervisor(t *testing.T, us ...*unit.Unit) *Supervisor {
	t.Helper()
	d := pid1.NewDispatcher()
	done := make(chan struct{})
	d.Start(done)
	s, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		close(done)
		t.Fatal(err)
	}
	t.Cleanup(func() { close(done); <-d.Done() })
	return s
}

func auditWait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAuditCompletedSpawnsReleaseOutputDescriptors(t *testing.T) {
	runtime.GC()
	old := debug.SetGCPercent(-1)
	defer func() { debug.SetGCPercent(old); runtime.GC() }()
	u := &unit.Unit{Name: "fd-audit.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		ExecStart: []string{"/bin/true"}}
	s := auditSupervisor(t, u)
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for range 32 {
		if err := s.spawnAndWait(u, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Child output pumps have already reached EOF. Resource ownership must
	// not depend on a future GC cycle (low RLIMIT_NOFILE can precede it).
	time.Sleep(50 * time.Millisecond)
	if after := count(); after > before+4 {
		t.Errorf("32 completed spawns left %d additional descriptors (before=%d after=%d)", after-before, before, after)
	}
}

// Simulate a cgroup tree becoming unavailable after the manager's successful
// startup probe. The private mount exists only inside the test container.
func TestAuditCgroupPlacementFailureStillKillsProcess(t *testing.T) {
	if os.Getenv("CONTAINER_INIT_AUDIT_PRIVILEGED") != "1" {
		t.Skip("set CONTAINER_INIT_AUDIT_PRIVILEGED=1 only in a disposable privileged container")
	}
	cg := cgroup.New()
	if os.Geteuid() != 0 || !cg.Available() {
		t.Skip("needs root and writable cgroup v2 in an isolated container")
	}
	if err := syscall.Mount("audit", cg.Base(), "tmpfs", syscall.MS_RDONLY, "mode=0555"); err != nil {
		t.Skipf("needs a private privileged mount namespace: %v", err)
	}
	defer syscall.Unmount(cg.Base(), 0)
	up := filepath.Join(t.TempDir(), "up")
	u := &unit.Unit{Name: "fallback-audit.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/sh", "-c", "trap '' TERM; touch " + up + "; exec sleep 60"}}
	s := auditSupervisor(t, u)
	s.cgroup = cg
	s.SetStopTimeout(100 * time.Millisecond)
	done := make(chan struct{})
	go func() { s.spawnAndWait(u, nil, nil); close(done) }()
	waitFile(t, up)
	s.mu.Lock()
	pid := s.services[u.Name].pid
	s.mu.Unlock()
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL); <-done }()
	s.Stop()
	s.shutdown()
	time.Sleep(50 * time.Millisecond)
	if processAlive(pid) {
		t.Errorf("process %d survived forced shutdown after cgroup placement failed (advertised PGID fallback did not run)", pid)
	}
}

// A trace destination can stall independently of the processes being
// stopped. A full FIFO blocks a spawn's trace write; shutdown must still
// stop running services within its deadline, so the write must not hold
// the spawn gate. (Tracing itself stays synchronous: the shutdown's own
// closing record waits for the destination.)
func TestAuditShutdownDeadlineIncludesSpawnGate(t *testing.T) {
	live := &unit.Unit{Name: "live-audit.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/sleep", "60"}}
	pending := &unit.Unit{Name: "pending-audit.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/true", strings.Repeat("x", 96*1024)}}
	s := auditSupervisor(t, live, pending)
	s.SetStopTimeout(100 * time.Millisecond)
	liveDone, spawned := make(chan struct{}), make(chan struct{})
	go func() { s.spawnAndWait(live, nil, func() { close(spawned) }); close(liveDone) }()
	select {
	case <-spawned:
	case <-time.After(3 * time.Second):
		t.Fatal("live service did not start")
	}
	fifo := filepath.Join(t.TempDir(), "trace")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	t.Setenv("CONTAINER_INIT_TRACE", "1")
	t.Setenv("CONTAINER_INIT_TRACE_FILE", fifo)
	s.tracer = trace.New()
	defer s.tracer.Close()
	// Unblock the injected I/O fault before any other cleanup, even on
	// failure: the tracer's Close waits for the blocked write.
	drained := make(chan struct{})
	release := sync.OnceFunc(func() { go func() { io.Copy(io.Discard, r); close(drained) }() })
	defer release()
	pendingDone := make(chan struct{})
	go func() { s.spawnAndWait(pending, nil, nil); close(pendingDone) }()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		recorded := s.services[pending.Name] != nil
		s.mu.Unlock()
		if recorded {
			break
		}
		if time.Now().After(deadline) {
			release()
			<-pendingDone
			t.Fatal("pending spawn was not recorded while its trace write was blocked")
		}
	}
	time.Sleep(50 * time.Millisecond) // into its ~96 KiB trace write
	select {
	case <-pendingDone:
		t.Fatal("pending spawn's trace write did not block; the fault was not injected")
	default:
	}
	s.Stop()
	stopped := make(chan struct{})
	go func() { s.shutdown(); close(stopped) }()
	exceeded := false
	select {
	case <-liveDone:
	case <-time.After(350 * time.Millisecond):
		exceeded = true
	}
	release()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown failed to recover after trace drain")
	}
	<-pendingDone
	<-liveDone
	s.tracer.Close()
	r.Close()
	<-drained
	if exceeded {
		t.Error("100ms shutdown deadline was exceeded while a spawn's trace write was blocked")
	}
}
