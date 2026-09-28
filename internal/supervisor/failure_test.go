//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

// TestStartFailureSettlesDependents: a unit whose ExecStart cannot be
// exec'd must release After=-only dependents and fail Requires=
// dependents, instead of leaving both waiting forever.
func TestStartFailureSettlesDependents(t *testing.T) {
	dir := t.TempDir()
	orderedMark := filepath.Join(dir, "ordered")
	requiringMark := filepath.Join(dir, "requiring")

	broken := &unit.Unit{
		Name:      "broken.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{filepath.Join(dir, "does-not-exist")},
		Restart:   unit.RestartNo,
	}
	ordered := &unit.Unit{
		Name:      "ordered.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"broken.service"},
		ExecStart: []string{"/bin/touch", orderedMark},
	}
	requiring := &unit.Unit{
		Name:      "requiring.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"broken.service"},
		Requires:  []string{"broken.service"},
		ExecStart: []string{"/bin/touch", requiringMark},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{broken, ordered, requiring}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	for _, name := range []string{"ordered.service", "requiring.service"} {
		select {
		case <-sup.ready[name]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
	// ordered.service is a oneshot: ready means it ran.
	if _, err := os.Stat(orderedMark); err != nil {
		t.Errorf("After=-only dependent did not run: %v", err)
	}
	if _, err := os.Stat(requiringMark); err == nil {
		t.Error("Requires= dependent ran despite its requirement failing")
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !sup.failed["broken.service"] || !sup.failed["requiring.service"] {
		t.Errorf("failed = %v, want broken.service and requiring.service", sup.failed)
	}
}

// TestNonRootUserUnits runs only without root (CI's runner user, or a
// container started with --user): a User= unit resolving to our own
// identity runs as-is, and one wanting another uid fails to start.
func TestNonRootUserUnits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root euid")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "ran")
	uid, gid := strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid())
	same := &unit.Unit{
		Name:      "same.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		User:      uid,
		Group:     gid,
		ExecStart: []string{"/bin/touch", mark},
	}
	other := &unit.Unit{
		Name:      "other.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		User:      strconv.Itoa(os.Geteuid() + 1),
		Group:     gid,
		ExecStart: []string{"/bin/true"},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{same, other}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	for _, name := range []string{"same.service", "other.service"} {
		select {
		case <-sup.ready[name]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("same-identity User= unit did not run: %v", err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.failed["same.service"] || !sup.failed["other.service"] {
		t.Errorf("failed = %v, want only other.service", sup.failed)
	}
}
