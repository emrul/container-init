package systemd1shim

import (
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

// captureEmitter records every signal the manager emits so tests can
// assert on order, count, and payload without spinning up a real bus.
type captureEmitter struct {
	mu     sync.Mutex
	events []emitted
}

type emitted struct {
	path   dbus.ObjectPath
	name   string
	values []any
}

func (c *captureEmitter) Emit(path dbus.ObjectPath, name string, values ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, emitted{path: path, name: name, values: values})
	return nil
}

func (c *captureEmitter) snapshot() []emitted {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]emitted, len(c.events))
	copy(out, c.events)
	return out
}

func TestStartTransientUnitEmitsJobNewAndJobRemoved(t *testing.T) {
	emitter := &captureEmitter{}
	m := newManager(emitter, nil)

	props := []property{
		{Name: "Description", Value: dbus.MakeVariant("ptyxis-spawn-xyz")},
		{Name: "PIDs", Value: dbus.MakeVariant([]uint32{1234})},
	}
	jobPath, dbusErr := m.StartTransientUnit("ptyxis-spawn-xyz.scope", "fail", props, nil)
	if dbusErr != nil {
		t.Fatalf("StartTransientUnit returned dbus error: %v", dbusErr)
	}
	if jobPath != "/org/freedesktop/systemd1/job/1" {
		t.Errorf("unexpected job path: %s", jobPath)
	}

	events := emitter.snapshot()
	if len(events) != 2 {
		t.Fatalf("expected 2 signals, got %d: %+v", len(events), events)
	}
	if events[0].name != managerIface+".JobNew" {
		t.Errorf("first signal should be JobNew, got %s", events[0].name)
	}
	if events[1].name != managerIface+".JobRemoved" {
		t.Errorf("second signal should be JobRemoved, got %s", events[1].name)
	}
	// JobRemoved's result arg is what systemd-run gates on -- "done"
	// means success. Anything else and systemd-run reports failure to
	// its caller, which is the exact failure mode we're paid to avoid.
	if got := events[1].values[3]; got != "done" {
		t.Errorf("JobRemoved result = %v, want \"done\"", got)
	}
	// The same unit name and job path appear in both signals.
	if events[0].values[1] != jobPath || events[1].values[1] != jobPath {
		t.Errorf("job path mismatch across signals")
	}
	if events[0].values[2] != "ptyxis-spawn-xyz.scope" || events[1].values[2] != "ptyxis-spawn-xyz.scope" {
		t.Errorf("unit name mismatch across signals")
	}
}

func TestSynthesizedJobIdsMonotonic(t *testing.T) {
	emitter := &captureEmitter{}
	m := newManager(emitter, nil)

	for i := 1; i <= 4; i++ {
		var jobPath dbus.ObjectPath
		var err *dbus.Error
		switch i % 4 {
		case 1:
			jobPath, err = m.StartTransientUnit("u.scope", "fail", nil, nil)
		case 2:
			jobPath, err = m.StartUnit("u.service", "replace")
		case 3:
			jobPath, err = m.StopUnit("u.service", "replace")
		case 0:
			jobPath, err = m.RestartUnit("u.service", "replace")
		}
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		want := dbus.ObjectPath("/org/freedesktop/systemd1/job/" + uint32ToA(uint32(i)))
		if jobPath != want {
			t.Errorf("iter %d: got %s, want %s", i, jobPath, want)
		}
	}
}

func TestGetUnitEscapesNonAlphanumerics(t *testing.T) {
	m := newManager(&captureEmitter{}, nil)
	got, dbusErr := m.GetUnit("my-app.service")
	if dbusErr != nil {
		t.Fatal(dbusErr)
	}
	// The "-" and "." are escaped to _XX hex per systemd's bus-path
	// convention. We don't round-trip this, but stability matters for
	// any caller that does property reads keyed off the returned path.
	want := dbus.ObjectPath("/org/freedesktop/systemd1/unit/my_2dapp_2eservice")
	if got != want {
		t.Errorf("GetUnit escape mismatch: got %s, want %s", got, want)
	}
}

func TestNoArgMethodsReturnNil(t *testing.T) {
	m := newManager(&captureEmitter{}, nil)
	if err := m.Subscribe(); err != nil {
		t.Errorf("Subscribe: %v", err)
	}
	if err := m.Unsubscribe(); err != nil {
		t.Errorf("Unsubscribe: %v", err)
	}
	if err := m.Reload(); err != nil {
		t.Errorf("Reload: %v", err)
	}
	if err := m.Reexecute(); err != nil {
		t.Errorf("Reexecute: %v", err)
	}
}

func TestDedupePreservesOrder(t *testing.T) {
	got := dedupe([]string{"system", "user:1000", "system", "", "  user:1000  ", "unix:path=/x"})
	want := []string{"system", "user:1000", "unix:path=/x"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}
