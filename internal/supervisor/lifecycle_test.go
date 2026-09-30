//go:build linux

package supervisor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/userdb"
	"github.com/emrul/container-init/unit"
)

// bareSupervisor provides the real dispatcher without eager boot activation.
func bareSupervisor(t *testing.T, us ...*unit.Unit) *Supervisor {
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

func waitFor(t *testing.T, what string, f func() bool) {
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

func TestConcurrentOnFailureShutdownOwnsEveryProcess(t *testing.T) {
	u := &unit.Unit{Name: "shared-handler.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		ExecStart: []string{"/bin/sleep", "60"}}
	s := bareSupervisor(t, u)
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() { s.fireOnFailure(u.Name); close(firstDone) }()
	var first int
	waitFor(t, "first failure handler", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if st := s.services[u.Name]; st != nil {
			first = st.pid
		}
		return first > 0
	})
	defer func() { _ = syscall.Kill(first, syscall.SIGKILL); <-firstDone }()
	go func() { s.fireOnFailure(u.Name); close(secondDone) }()
	// The second invocation may coalesce or wait its turn; either way
	// shutdown must still stop the first one's process.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		latest := s.services[u.Name].pid
		s.mu.Unlock()
		if latest != first {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	s.shutdown()
	// Give the dispatcher time to reap anything actually signalled.
	time.Sleep(50 * time.Millisecond)
	if processAlive(first) {
		t.Errorf("first OnFailure process %d survived shutdown after its state record was replaced", first)
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Error("second invocation did not finish")
	}
}

func TestCompletedSpawnsReleaseOutputDescriptors(t *testing.T) {
	runtime.GC()
	old := debug.SetGCPercent(-1)
	defer func() { debug.SetGCPercent(old); runtime.GC() }()
	u := &unit.Unit{Name: "fd.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		ExecStart: []string{"/bin/true"}}
	s := bareSupervisor(t, u)
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

func TestGroupWithoutUserIsEnforced(t *testing.T) {
	u := &unit.Unit{Name: "group-only.service", Kind: unit.KindService, Type: unit.TypeOneshot}
	out := filepath.Join(t.TempDir(), "gid")
	u.ExecStart = []string{"/bin/sh", "-c", "id -g > " + out}
	u.Group = strconv.Itoa(os.Getegid() + 1)
	s := bareSupervisor(t, u)
	err := s.spawnAndWait(u, nil, nil)
	if os.Geteuid() != 0 {
		if err == nil {
			t.Error("non-root supervisor silently ran a unit requesting a different Group=")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != u.Group {
		t.Errorf("Group=%s ran with gid %s", u.Group, strings.TrimSpace(string(got)))
	}
}

func TestUserIdentityEnvironmentWinsDuplicates(t *testing.T) {
	t.Setenv("HOME", "/inherited")
	t.Setenv("USER", "inherited")
	t.Setenv("LOGNAME", "inherited")
	id, err := userdb.Resolve(strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid()), "")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "identity")
	u := &unit.Unit{Name: "identity-env.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		User: strconv.Itoa(os.Geteuid()), Group: strconv.Itoa(os.Getegid()),
		Environment: []string{"HOME=/directive", "USER=directive", "LOGNAME=directive"},
		ExecStart:   []string{"/bin/sh", "-c", `printf '%s|%s|%s' "$HOME" "$USER" "$LOGNAME" > ` + out}}
	s := bareSupervisor(t, u)
	if err := s.spawnAndWait(u, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%s|%s|%s", id.Home, id.Username, id.Username); string(got) != want {
		t.Errorf("identity environment = %q, want %q (spawnAndWait promises User= wins)", got, want)
	}
}

func TestSocketModeIsEnforced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "private.socket")
	address := filepath.Join(dir, "private.sock")
	if err := os.WriteFile(p, []byte("[Socket]\nListenStream="+address+"\nSocketMode=0600\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, warnings, err := unit.LoadFile(p, unit.Options{Strict: true})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("load: %v, warnings=%v", err, warnings)
	}
	s := bareSupervisor(t, u)
	s.bindSocket(u)
	defer s.closeSocket(u.Name)
	st, err := os.Stat(address)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("SocketMode=0600 produced %04o", st.Mode().Perm())
	}
}

// Simulate a cgroup tree becoming unavailable after the manager's successful
// startup probe. The private mount exists only inside the test container.
func TestCgroupPlacementFailureStillKillsProcess(t *testing.T) {
	if os.Getenv("CONTAINER_INIT_TEST_PRIVILEGED") != "1" {
		t.Skip("set CONTAINER_INIT_TEST_PRIVILEGED=1 only in a disposable privileged container")
	}
	cg := cgroup.New()
	if os.Geteuid() != 0 || !cg.Available() {
		t.Skip("needs root and writable cgroup v2 in an isolated container")
	}
	if err := syscall.Mount("cgroup-test", cg.Base(), "tmpfs", syscall.MS_RDONLY, "mode=0555"); err != nil {
		t.Skipf("needs a private privileged mount namespace: %v", err)
	}
	defer syscall.Unmount(cg.Base(), 0)
	up := filepath.Join(t.TempDir(), "up")
	u := &unit.Unit{Name: "fallback.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/sh", "-c", "trap '' TERM; touch " + up + "; exec sleep 60"}}
	s := bareSupervisor(t, u)
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

func TestFailedSocketBlocksRequiredService(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	mark := filepath.Join(t.TempDir(), "consumer-ran")
	sock := &unit.Unit{Name: "occupied.socket", Kind: unit.KindSocket, Service: "helper.service",
		ListenStream: []unit.Listener{{Network: "tcp", Address: occupied.Addr().String()}}}
	helper := &unit.Unit{Name: "helper.service", Kind: unit.KindService, Type: unit.TypeSimple, ExecStart: []string{"/bin/sleep", "60"}}
	consumer := &unit.Unit{Name: "consumer.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		After: []string{sock.Name}, Requires: []string{sock.Name}, ExecStart: []string{"/bin/touch", mark}}
	s := runUnits(t, sock, helper, consumer)
	waitSettled(t, s, consumer.Name)
	if _, err := os.Stat(mark); err == nil {
		t.Error("consumer ran although its ordered Requires= socket failed to bind")
	}
}

func TestOnFailureHonoursMissingRequirement(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "handler-ran")
	handler := &unit.Unit{Name: "handler.service", Kind: unit.KindService, Type: unit.TypeOneshot,
		Requires: []string{"not-installed.service"}, ExecStart: []string{"/bin/touch", mark}}
	s := bareSupervisor(t, handler)
	s.fireOnFailure(handler.Name)
	if _, err := os.Stat(mark); err == nil {
		t.Error("OnFailure handler ran despite a missing Requires= unit")
	}
}
