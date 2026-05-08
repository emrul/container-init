//go:build linux

package cgroup

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestManagerLifecycle exercises detect → Mkdir → Place → Kill →
// Remove against the real cgroup-v2 mount. Skips when cgroup-v2 is
// not usable here (most often: macOS dev hosts; cgroup-v1 hosts; or
// tightly-restricted CI runners that don't delegate the cgroup).
func TestManagerLifecycle(t *testing.T) {
	m := New()
	if !m.Available() {
		t.Skipf("cgroup-v2 not available: %v", m.Err())
	}
	defer m.Remove("test-unit.service")

	if _, err := m.Mkdir("test-unit.service"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	if err := m.Place("test-unit.service", pid); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !m.HasMembers("test-unit.service") {
		t.Errorf("HasMembers = false after Place")
	}
	if err := m.Kill("test-unit.service"); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil && err == syscall.ESRCH {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("pid %d still alive after cgroup.kill", pid)
	}
}

// TestManagerNoopWhenUnavailable pins the fallback contract: every op
// returns a non-nil error and never panics when Available is false.
func TestManagerNoopWhenUnavailable(t *testing.T) {
	m := &Manager{} // not New() — explicit unavailable manager
	if m.Available() {
		t.Fatal("zero-value manager should report Available=false")
	}
	if _, err := m.Mkdir("foo.service"); err == nil {
		t.Errorf("Mkdir on unavailable manager: want error, got nil")
	}
	if err := m.Place("foo.service", 1); err == nil {
		t.Errorf("Place on unavailable manager: want error, got nil")
	}
	if err := m.Kill("foo.service"); err == nil {
		t.Errorf("Kill on unavailable manager: want error, got nil")
	}
	if m.HasMembers("foo.service") {
		t.Errorf("HasMembers on unavailable manager: want false")
	}
	if err := m.Remove("foo.service"); err != nil {
		t.Errorf("Remove on unavailable manager: want nil, got %v", err)
	}
}

// TestSanitizePassesThrough pins today's behaviour — unit names round-trip.
func TestSanitizePassesThrough(t *testing.T) {
	for _, name := range []string{"foo.service", "web.service", "audio-out-ws.socket"} {
		if got := sanitize(name); got != name {
			t.Errorf("sanitize(%q) = %q, want %q", name, got, name)
		}
		if strings.ContainsAny(got, "/") {
			t.Errorf("sanitize(%q) contains path separator", name)
		}
	}
}
