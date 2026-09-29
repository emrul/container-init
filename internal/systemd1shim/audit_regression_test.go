package systemd1shim

import "testing"

// Distinct unit names must not share a state record or stop one another.
func TestAuditUnitNameEscapingIsInjective(t *testing.T) {
	m := newManager(&captureEmitter{}, nil)
	const first, second = "app-a.scope", "app_2da.scope"
	m.StartUnit(first, "replace")
	m.StartUnit(second, "replace")
	p := newPropertyStub(m.units)
	for _, name := range []string{first, second} {
		got, err := p.Get(msgAt(unitPath(name)), unitIface, "Id")
		if err != nil || got.Value() != name {
			t.Errorf("Id for %s = %v (%v), want its own name", name, got.Value(), err)
		}
	}
	m.StopUnit(second, "replace")
	got, _ := p.Get(msgAt(unitPath(first)), unitIface, "ActiveState")
	if got.Value() != "active" {
		t.Errorf("stopping %s changed %s to %q", second, first, got.Value())
	}
}
