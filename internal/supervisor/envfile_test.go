//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

// TestEnvironmentFileLoaded writes a key=value file, points a unit at
// it, and verifies the value reaches the child's environment. Without
// EnvironmentFile= support in the supervisor (the gap this test
// guards), the child sees the var unset and writes the literal "MISS"
// instead of the expected value.
func TestEnvironmentFileLoaded(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "kasm-dbus.env")
	out := filepath.Join(dir, "out")
	// Single-quoted value, matching dbus-launch --sh-syntax. The
	// parser handles double-quoted, single-quoted, and unquoted; this
	// is the form Kasm's /tmp/kasm-dbus.env actually uses.
	envContent := "DBUS_SESSION_BUS_ADDRESS='unix:path=/tmp/dbus-test,guid=deadbeef'\nUNRELATED=ok\n"
	if err := os.WriteFile(envFile, []byte(envContent), 0o644); err != nil {
		t.Fatal(err)
	}

	u := &unit.Unit{
		Name:            "envfile-probe.service",
		Kind:            unit.KindService,
		Type:            unit.TypeOneshot,
		ExecStart:       []string{"/bin/sh", "-c", `printf '%s' "${DBUS_SESSION_BUS_ADDRESS:-MISS}" > ` + out},
		EnvironmentFile: []unit.EnvFileRef{{Path: envFile}},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{u}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()

	// Oneshot exits on its own; wait up to 3s for the artifact then stop.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(out); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	sup.Stop()
	<-runDone

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("oneshot did not write output: %v", err)
	}
	want := "unix:path=/tmp/dbus-test,guid=deadbeef"
	if string(got) != want {
		t.Errorf("child saw DBUS_SESSION_BUS_ADDRESS=%q, want %q", got, want)
	}
}

// TestEnvironmentFileMissingFailsUnit verifies that a missing,
// non-IgnoreMissing env file fails the unit rather than running it
// with whatever stale env happened to be inherited.
func TestEnvironmentFileMissingFailsUnit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	u := &unit.Unit{
		Name:            "missing-env.service",
		Kind:            unit.KindService,
		Type:            unit.TypeOneshot,
		ExecStart:       []string{"/bin/sh", "-c", "touch " + out},
		EnvironmentFile: []unit.EnvFileRef{{Path: filepath.Join(dir, "nope.env")}},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, _ := New([]*unit.Unit{u}, nil, d, &cgroup.Manager{})
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	time.Sleep(200 * time.Millisecond)
	sup.Stop()
	<-runDone

	if _, err := os.Stat(out); err == nil {
		t.Errorf("ExecStart ran despite missing required env file")
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error: %v", err)
	}
}

// TestEnvironmentFileIgnoreMissingSkipped confirms `-/path` semantics:
// a missing file marked IgnoreMissing doesn't fail the unit; the
// ExecStart still runs (just without those vars).
func TestEnvironmentFileIgnoreMissingSkipped(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	u := &unit.Unit{
		Name:      "ignore-missing.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{"/bin/sh", "-c", "printf RAN > " + out},
		EnvironmentFile: []unit.EnvFileRef{
			{Path: filepath.Join(dir, "absent.env"), IgnoreMissing: true},
		},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, _ := New([]*unit.Unit{u}, nil, d, &cgroup.Manager{})
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(out); err == nil && strings.TrimSpace(string(data)) == "RAN" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	sup.Stop()
	<-runDone

	got, _ := os.ReadFile(out)
	if string(got) != "RAN" {
		t.Errorf("ExecStart did not run with IgnoreMissing env file (out=%q)", got)
	}
}

// TestEnvFileOverridesEnvironmentDirective asserts systemd's
// precedence (systemd.exec(5)): settings from EnvironmentFile= override
// Environment=, and a later file overrides an earlier one. A variable
// set only by Environment= still comes through.
func TestEnvFileOverridesEnvironmentDirective(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(first, []byte("WHO=first\nWHAT=first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("WHAT=second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &unit.Unit{
		Name:            "precedence.service",
		Kind:            unit.KindService,
		Type:            unit.TypeOneshot,
		ExecStart:       []string{"/bin/sh", "-c", "printf '%s %s %s' \"$WHO\" \"$WHAT\" \"$ONLY\" > " + out},
		EnvironmentFile: []unit.EnvFileRef{{Path: first}, {Path: second}},
		Environment:     []string{"WHO=directive", "WHAT=directive", "ONLY=directive"},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, _ := New([]*unit.Unit{u}, nil, d, &cgroup.Manager{})
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(out); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	sup.Stop()
	<-runDone

	got, _ := os.ReadFile(out)
	if want := "first second directive"; string(got) != want {
		t.Errorf("child saw WHO WHAT ONLY = %q, want %q", got, want)
	}
}
