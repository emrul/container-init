package systemd1shim

import (
	"github.com/godbus/dbus/v5"
)

// unitIface is the per-unit interface whose properties clients read.
const unitIface = "org.freedesktop.systemd1.Unit"

// propertyStub answers org.freedesktop.DBus.Properties on per-unit
// paths under /org/freedesktop/systemd1/unit/. systemd-run reads
// InvocationID right after StartTransientUnit completes; without a
// responder it logs "Failed to request invocation ID for unit" and
// exits non-zero, which Ptyxis surfaces as "Terminal (Failed)".
//
// Units the shim started answer a small typed table on
// org.freedesktop.systemd1.Unit -- loaded, active, running. Chrome
// starts app-<id>-<pid>.scope, reads GetAll on it, and holds its file
// dialogs until ActiveState is "active". The unit is active before
// its path is handed out, so no PropertiesChanged is needed. Every
// other path and interface gets zero values.
type propertyStub struct {
	units *unitSet
}

func newPropertyStub(units *unitSet) *propertyStub { return &propertyStub{units: units} }

// activeUnitProperties is the table a started unit answers with. The
// values are typed as systemd documents them; all four are `s`.
func activeUnitProperties(name string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Id":          dbus.MakeVariant(name),
		"LoadState":   dbus.MakeVariant("loaded"),
		"ActiveState": dbus.MakeVariant("active"),
		"SubState":    dbus.MakeVariant("running"),
	}
}

// started returns the property table for the unit at msg's path, when
// the shim started that unit and iface is the Unit interface.
func (p *propertyStub) started(msg dbus.Message, iface string) (map[string]dbus.Variant, bool) {
	if iface != unitIface {
		return nil, false
	}
	path, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	name, ok := p.units.lookup(path)
	if !ok {
		return nil, false
	}
	return activeUnitProperties(name), true
}

// Get returns the table value for a started unit, else a zero-typed
// variant. InvocationID is documented as `ay` (128-bit uuid); we
// return an empty byte slice which marshals as `ay` of length 0 --
// callers that compare against an empty value (the failure sentinel)
// move on.
func (p *propertyStub) Get(msg dbus.Message, iface, name string) (dbus.Variant, *dbus.Error) {
	if props, ok := p.started(msg, iface); ok {
		if v, ok := props[name]; ok {
			return v, nil
		}
	}
	switch name {
	case "InvocationID":
		return dbus.MakeVariant([]byte{}), nil
	default:
		return dbus.MakeVariant(""), nil
	}
}

// GetAll returns the table for a started unit on the Unit interface,
// else an empty dict -- "this unit has no observable state."
func (p *propertyStub) GetAll(msg dbus.Message, iface string) (map[string]dbus.Variant, *dbus.Error) {
	if props, ok := p.started(msg, iface); ok {
		return props, nil
	}
	return map[string]dbus.Variant{}, nil
}

// Set accepts any write and stores nothing: returning an error would
// break callers that optimistically write properties they do not need
// persisted.
func (p *propertyStub) Set(iface, name string, value dbus.Variant) *dbus.Error {
	return nil
}
