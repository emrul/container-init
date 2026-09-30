// Package cgroup wraps the cgroup-v2 subset container-init relies on
// for atomic process-tree teardown. A per-unit cgroup is bound to its
// processes, not their ids, so writing 1 to cgroup.kill SIGKILLs every
// member atomically, immune to the PID reuse that makes a delayed
// kill(-pgid) unsafe.
//
// Layout: <our-cgroup>/container-init/<unit>/, made once per unit. The
// supervisor spawns the unit's process directly into it (clone3 with
// CLONE_INTO_CGROUP; Place is the fallback for kernels without it), so
// its descendants inherit the cgroup.
//
// When cgroup-v2 is not mounted or not writable, the Manager is a
// no-op and the supervisor kills by process group instead; Err() says
// why.
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
	// cgroupRoot is the cgroup-v2 unified-hierarchy mountpoint.
	cgroupRoot = "/sys/fs/cgroup"
	procFile   = "/proc/self/cgroup"
	// initSub is the per-instance subdirectory we own under the
	// container's own cgroup. Per-unit leaves go below this.
	initSub = "container-init"
)

// Manager owns the per-unit cgroup directories. Check Available()
// before relying on Mkdir / Place / Kill to do real work.
type Manager struct {
	available bool
	base      string
	err       error

	mu    sync.Mutex
	units map[string]string // unit name -> absolute cgroup path
}

// New initialises the manager; it is never nil. Available() and Err()
// say whether cgroup-v2 is usable here.
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

// Mkdir creates the per-unit cgroup directory, if it does not exist
// yet, and returns its absolute path. It is idempotent, so it is
// called before every spawn.
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
// Processes it forks from then on inherit the cgroup; any it forked
// before stay where they are.
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
// currently in the cgroup. cgroup.kill needs kernel 5.14 or later.
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

// HasMembers reports whether unit's cgroup has at least one process
// in it.
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

// Remove deletes the per-unit cgroup directory. On a cgroup that
// still has members rmdir(2) fails with EBUSY, which is returned so
// the caller can retry after Kill.
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

// detect locates container-init's own cgroup-v2 directory and creates
// the container-init/ subdirectory under it. An error means cgroup-v2
// is not usable here.
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

// sanitize maps a unit name to its cgroup directory name. Unit names
// are already filesystem-safe, so the mapping is the identity.
func sanitize(name string) string { return name }
