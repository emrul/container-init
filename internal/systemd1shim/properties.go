package systemd1shim

import (
	"github.com/godbus/dbus/v5"
)

// propertyStub answers org.freedesktop.DBus.Properties on per-unit
// paths under /org/freedesktop/systemd1/unit/. systemd-run reads
// InvocationID right after StartTransientUnit completes; without a
// responder it logs "Failed to request invocation ID for unit" and
// exits non-zero, which Ptyxis surfaces as "Terminal (Failed)".
//
// We don't track unit state, so all queries return zero values --
// good enough for callers that consume the answer for logging only.
type propertyStub struct{}

func newPropertyStub() *propertyStub { return &propertyStub{} }

// Get returns a zero-typed variant for the requested property.
// InvocationID is documented as `ay` (128-bit uuid); we return an
// empty byte slice which marshals as `ay` of length 0 -- callers
// that compare against an empty value (the failure sentinel) move on.
func (p *propertyStub) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	switch name {
	case "InvocationID":
		return dbus.MakeVariant([]byte{}), nil
	default:
		return dbus.MakeVariant(""), nil
	}
}

// GetAll returns an empty dict. Callers iterating over all properties
// see no entries -- equivalent to "this unit has no observable state."
func (p *propertyStub) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	return map[string]dbus.Variant{}, nil
}

// Set silently accepts any write. We don't store unit state, so this
// is a no-op -- but returning an error would break callers that
// optimistically write properties they don't really need persisted.
func (p *propertyStub) Set(iface, name string, value dbus.Variant) *dbus.Error {
	return nil
}
