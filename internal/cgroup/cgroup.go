// Package cgroup wraps the cgroup-v2 subset container-init relies on
// for atomic process-tree teardown. Closes the original design surprise
// #3 (Linux PID/PGID reuse defeats delayed kill -PGID): a per-unit
// cgroup is identity-bound, so writing 1 to cgroup.kill atomically
// SIGKILLs every member regardless of PID reuse races.
//
// Layout: container-init mkdirs <our-cgroup>/container-init/<unit>/
// once per unit, places the spawned PID (and therefore its descendants
// via inheritance) into that cgroup, and on shutdown writes 1 to
// cgroup.kill to take everything out atomically.
//
// Falls back to a no-op manager when cgroup-v2 is not mounted or not
// writable here — the supervisor then relies on the legacy SIGTERM
// path. Detection is best-effort and recorded in Err() for trace.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// cgroupRoot is the cgroup-v2 unified-hierarchy mountpoint on
	// every distro in the Phase 1 matrix.
	cgroupRoot = "/sys/fs/cgroup"
	procFile   = "/proc/self/cgroup"
	// initSub is the per-instance subdirectory we own under the
	// container's own cgroup. Per-unit leaves go below this.
	initSub = "container-init"
)

// Manager owns the per-unit cgroup directories. Always returns a
// non-nil instance; check Available() before relying on Mkdir / Place
// / Kill to do real work.
type Manager struct {
	available bool
	base      string
	err       error

	mu    sync.Mutex
	units map[string]string // unit name -> absolute cgroup path
}

// New initialises the manager. A nil error from this constructor is
// not the success signal — call Available() / Err() after to learn
// whether cgroup-v2 is reachable here.
func New() *Manager {
	m := &Manager{units: make(map[string]string)}
	base, err := detect()
	if err != nil {
		m.err = err
		return m
	}
	m.base = base
	m.available = true
	return m
}

// Available reports whether the manager will perform real cgroup
// operations. False means cgroup-v2 is unmounted, the container's
// cgroup wasn't found, or our base directory wasn't writable.
func (m *Manager) Available() bool { return m.available }

// Err returns the detection failure that disabled the manager (nil
// when Available()).
func (m *Manager) Err() error { return m.err }

// Base returns the absolute path under which per-unit cgroups are
// created (empty string when !Available).
func (m *Manager) Base() string { return m.base }

// Mkdir creates (idempotently) the per-unit cgroup directory and
// returns its absolute path. Safe to call before every spawn —
// subsequent calls just look up the existing entry.
func (m *Manager) Mkdir(unit string) (string, error) {
	if !m.available {
		return "", errors.New("cgroup: not available")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.units[unit]; ok {
		return p, nil
	}
	p := filepath.Join(m.base, sanitize(unit))
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", p, err)
	}
	m.units[unit] = p
	return p, nil
}

// Place migrates pid into unit's cgroup by writing to cgroup.procs.
// Subsequent fork(2)s by the placed process inherit the cgroup
// automatically — so any double-fork descendants land in the same
// killable group.
func (m *Manager) Place(unit string, pid int) error {
	if !m.available {
		return errors.New("cgroup: not available")
	}
	m.mu.Lock()
	p, ok := m.units[unit]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("cgroup: unit %s not initialized; call Mkdir first", unit)
	}
	return os.WriteFile(filepath.Join(p, "cgroup.procs"), []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}

// Kill writes 1 to cgroup.kill, atomically SIGKILLing every process
// currently in the cgroup. Available since kernel 5.14 (predates the
// Phase 1 matrix's oldest base, Ubuntu Jammy 22.04 kernel 5.15).
func (m *Manager) Kill(unit string) error {
	if !m.available {
		return errors.New("cgroup: not available")
	}
	m.mu.Lock()
	p, ok := m.units[unit]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("cgroup: unit %s not initialized", unit)
	}
	return os.WriteFile(filepath.Join(p, "cgroup.kill"), []byte("1"), 0o644)
}

// HasMembers returns true when unit's cgroup currently has at least
// one process in it. Used by shutdown to decide whether to fire
// cgroup.kill.
func (m *Manager) HasMembers(unit string) bool {
	if !m.available {
		return false
	}
	m.mu.Lock()
	p, ok := m.units[unit]
	m.mu.Unlock()
	if !ok {
		return false
	}
	data, err := os.ReadFile(filepath.Join(p, "cgroup.procs"))
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

// Remove deletes the per-unit cgroup directory. Safe even when the
// cgroup is non-empty — rmdir(2) on a non-empty cgroup-v2 directory
// returns EBUSY which we surface so the caller can retry after
// Kill+drain.
func (m *Manager) Remove(unit string) error {
	if !m.available {
		return nil
	}
	m.mu.Lock()
	p, ok := m.units[unit]
	delete(m.units, unit)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return os.Remove(p)
}

// detect locates the writable cgroup-v2 dir for container-init's own
// cgroup and creates the container-init/ subdirectory under it.
// Failure here means cgroup-v2 isn't usable — the caller falls back
// to the legacy PGID path.
func detect() (string, error) {
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return "", fmt.Errorf("cgroup-v2 not mounted at %s: %w", cgroupRoot, err)
	}
	data, err := os.ReadFile(procFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", procFile, err)
	}
	var rel string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		// cgroup-v2 unified-hierarchy entries are "0::<path>".
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if rel == "" {
		return "", fmt.Errorf("no cgroup-v2 entry in %s", procFile)
	}
	base := filepath.Join(cgroupRoot, strings.TrimPrefix(rel, "/"), initSub)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", base, err)
	}
	return base, nil
}

// sanitize keeps unit-name → directory-name a 1:1 mapping today
// (".service" / ".socket" suffixes are filesystem-safe). Reserved as
// the future-proofing seam for any drop-in name that needs escaping.
func sanitize(name string) string { return name }
