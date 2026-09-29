package systemd1shim

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// msgAt builds the method-call message the stub sees for a property
// read on path; only the path header matters to it.
func msgAt(path dbus.ObjectPath) dbus.Message {
	return dbus.Message{Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath: dbus.MakeVariant(path),
	}}
}

// TestPropertyStubInvocationID locks in the behaviour systemd-run
// depends on: Properties.Get("...Unit", "InvocationID") must return
// without error (the value is zero-valued; systemd-run treats that as
// "no recorded invocation" and proceeds rather than aborting). It
// holds for a unit the shim started too, whose table has no
// InvocationID.
func TestPropertyStubInvocationID(t *testing.T) {
	units := newUnitSet(maxUnits)
	units.add("run-u1.scope")
	p := newPropertyStub(units)
	for _, path := range []dbus.ObjectPath{unitPath("other.scope"), unitPath("run-u1.scope")} {
		v, dErr := p.Get(msgAt(path), unitIface, "InvocationID")
		if dErr != nil {
			t.Fatalf("%s: Get InvocationID: %v", path, dErr)
		}
		if v.Signature().String() != "ay" {
			t.Errorf("%s: InvocationID signature = %s, want ay", path, v.Signature())
		}
		b, ok := v.Value().([]byte)
		if !ok {
			t.Fatalf("%s: InvocationID value type = %T, want []byte", path, v.Value())
		}
		if len(b) != 0 {
			t.Errorf("%s: InvocationID byte slice should be zero-length sentinel, got %d bytes", path, len(b))
		}
	}
}

// TestPropertyStubGetAllEmpty covers the GetAll path used by tools
// like `busctl introspect <path>` that enumerate every property, on a
// unit the shim did not start.
func TestPropertyStubGetAllEmpty(t *testing.T) {
	p := newPropertyStub(newUnitSet(maxUnits))
	got, dErr := p.GetAll(msgAt(unitPath("other.service")), unitIface)
	if dErr != nil {
		t.Fatal(dErr)
	}
	if len(got) != 0 {
		t.Errorf("GetAll = %v, want empty", got)
	}
}

// TestPropertyStubStartedUnit is what Chrome needs before it opens a
// file dialog: its app scope reads back loaded/active/running, from
// GetAll and from Get, on the Unit interface only.
func TestPropertyStubStartedUnit(t *testing.T) {
	const name = "app-com.google.Chrome-4242.scope"
	units := newUnitSet(maxUnits)
	units.add(name)
	p := newPropertyStub(units)
	msg := msgAt(unitPath(name))

	want := map[string]string{
		"Id":          name,
		"LoadState":   "loaded",
		"ActiveState": "active",
		"SubState":    "running",
	}
	got, dErr := p.GetAll(msg, unitIface)
	if dErr != nil {
		t.Fatal(dErr)
	}
	if len(got) != len(want) {
		t.Errorf("GetAll = %v, want %d entries", got, len(want))
	}
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("GetAll missing %s", k)
			continue
		}
		if v.Signature().String() != "s" || v.Value() != w {
			t.Errorf("GetAll %s = %v (%s), want %q (s)", k, v.Value(), v.Signature(), w)
		}
		gv, dErr := p.Get(msg, unitIface, k)
		if dErr != nil {
			t.Fatal(dErr)
		}
		if gv.Value() != w {
			t.Errorf("Get %s = %v, want %q", k, gv.Value(), w)
		}
	}

	for _, iface := range []string{"org.freedesktop.systemd1.Scope", "org.freedesktop.systemd1.Service"} {
		got, dErr := p.GetAll(msg, iface)
		if dErr != nil {
			t.Fatal(dErr)
		}
		if len(got) != 0 {
			t.Errorf("GetAll(%s) = %v, want empty", iface, got)
		}
		v, _ := p.Get(msg, iface, "ActiveState")
		if v.Value() != "" {
			t.Errorf("Get(%s, ActiveState) = %v, want \"\"", iface, v.Value())
		}
	}
}

// TestPropertyStubSetIsNoop ensures writes never raise; some callers
// optimistically set properties after StartTransientUnit and would
// abort on error.
func TestPropertyStubSetIsNoop(t *testing.T) {
	p := newPropertyStub(newUnitSet(maxUnits))
	if err := p.Set("any.iface", "AnyProp", dbus.MakeVariant("x")); err != nil {
		t.Errorf("Set returned error: %v", err)
	}
}

// TestUnitSetBounded drops the oldest unit once the set is full, and
// re-adding a known unit does not grow it.
func TestUnitSetBounded(t *testing.T) {
	s := newUnitSet(2)
	s.add("a.scope")
	s.add("b.scope")
	s.add("a.scope")
	s.add("c.scope")
	if _, ok := s.lookup(unitPath("a.scope")); ok {
		t.Error("a.scope should have been evicted")
	}
	for _, n := range []string{"b.scope", "c.scope"} {
		if got, ok := s.lookup(unitPath(n)); !ok || got != n {
			t.Errorf("lookup %s = %q, %v", n, got, ok)
		}
	}
	s.remove("b.scope")
	s.add("d.scope")
	if _, ok := s.lookup(unitPath("c.scope")); !ok {
		t.Error("c.scope evicted after a remove freed a slot")
	}
}

// TestManagerRecordsUnits: Start, StartTransient and Restart record a
// unit; Stop forgets it.
func TestManagerRecordsUnits(t *testing.T) {
	m := newManager(&captureEmitter{}, nil)
	m.StartTransientUnit("t.scope", "fail", nil, nil)
	m.StartUnit("s.service", "replace")
	m.RestartUnit("r.service", "replace")
	for _, n := range []string{"t.scope", "s.service", "r.service"} {
		if _, ok := m.units.lookup(unitPath(n)); !ok {
			t.Errorf("%s not recorded", n)
		}
	}
	m.StopUnit("s.service", "replace")
	if _, ok := m.units.lookup(unitPath("s.service")); ok {
		t.Error("s.service still recorded after StopUnit")
	}
	if p, _ := m.GetUnit("t.scope"); p != unitPath("t.scope") {
		t.Errorf("GetUnit path %s does not match the recorded key", p)
	}
}
