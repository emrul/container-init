//go:build linux

package systemd1shim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestStartTransientUnitOverWire registers the shim on a real
// dbus-daemon session bus running in a tempdir, then issues the same
// StartTransientUnit call shape that systemd-run --user --scope sends
// (Description s, PIDs au, AddRef b, CollectMode s).
//
// The intent is to catch godbus's reflective demarshalling bugs that
// the standalone manager tests miss -- those use an in-process
// emitter and never exercise the wire decode path.
//
// Skips if `dbus-daemon` isn't on PATH.
func TestStartTransientUnitOverWire(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skipf("dbus-daemon not on PATH: %v", err)
	}
	dir := t.TempDir()
	confPath := filepath.Join(dir, "session.conf")
	sockPath := filepath.Join(dir, "bus")
	conf := `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=` + sockPath + `</listen>
  <auth>EXTERNAL</auth>
  <policy context="default"><allow send_destination="*"/><allow receive_sender="*"/><allow own="*"/></policy>
</busconfig>
`
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("dbus-daemon", "--nofork", "--config-file="+confPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// Poll for the socket.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	addr := "unix:path=" + sockPath

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs []string
	var mu sync.Mutex
	shim, err := Open(ctx, []string{addr}, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	time.Sleep(300 * time.Millisecond) // let the shim RequestName

	client, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Auth(nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Hello(); err != nil {
		t.Fatal(err)
	}

	// systemd 255's systemd-run sends Pidfds (ah, array of fd indices)
	// instead of PIDs (au) when UNIX_FD is negotiated.
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{"Description", dbus.MakeVariant("Run: test")},
		{"PIDFDs", dbus.MakeVariant([]dbus.UnixFDIndex{})},
		{"AddRef", dbus.MakeVariant(true)},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
	}
	type auxT struct {
		Name       string
		Properties []struct {
			Name  string
			Value dbus.Variant
		}
	}
	aux := []auxT{}

	var jobPath dbus.ObjectPath
	mgr := client.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	err = mgr.Call("org.freedesktop.systemd1.Manager.StartTransientUnit", 0,
		"test.scope", "fail", props, aux).Store(&jobPath)
	if err != nil {
		mu.Lock()
		for _, l := range logs {
			t.Logf("shim: %s", l)
		}
		mu.Unlock()
		t.Fatalf("StartTransientUnit failed: %v", err)
	}
	if jobPath == "" {
		t.Fatal("empty job path")
	}
	t.Logf("got job path %s", jobPath)

	// Chrome's sequence for its app scope: GetUnit, then GetAll on the
	// Unit interface at the returned path. Its file dialogs wait for
	// ActiveState "active".
	var uPath dbus.ObjectPath
	if err := mgr.Call("org.freedesktop.systemd1.Manager.GetUnit", 0, "test.scope").Store(&uPath); err != nil {
		t.Fatalf("GetUnit: %v", err)
	}
	var got map[string]dbus.Variant
	unit := client.Object("org.freedesktop.systemd1", uPath)
	if err := unit.Call("org.freedesktop.DBus.Properties.GetAll", 0,
		"org.freedesktop.systemd1.Unit").Store(&got); err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if v := got["ActiveState"]; v.Value() != "active" {
		t.Errorf("ActiveState = %v, want active (all: %v)", v.Value(), got)
	}
	if v := got["Id"]; v.Value() != "test.scope" {
		t.Errorf("Id = %v, want test.scope", v.Value())
	}
	// systemd-run's InvocationID read still answers on the same path.
	var inv dbus.Variant
	if err := unit.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.systemd1.Unit", "InvocationID").Store(&inv); err != nil {
		t.Fatalf("Get InvocationID: %v", err)
	}
	if inv.Signature().String() != "ay" {
		t.Errorf("InvocationID signature = %s, want ay", inv.Signature())
	}
}

