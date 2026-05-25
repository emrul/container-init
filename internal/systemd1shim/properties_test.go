package systemd1shim

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestPropertyStubInvocationID locks in the behaviour systemd-run
// depends on: Properties.Get("...Unit", "InvocationID") must return
// without error (the value is zero-valued; systemd-run treats that as
// "no recorded invocation" and proceeds rather than aborting).
func TestPropertyStubInvocationID(t *testing.T) {
	p := newPropertyStub()
	v, dErr := p.Get("org.freedesktop.systemd1.Unit", "InvocationID")
	if dErr != nil {
		t.Fatalf("Get InvocationID: %v", dErr)
	}
	if v.Signature().String() != "ay" {
		t.Errorf("InvocationID signature = %s, want ay", v.Signature())
	}
	b, ok := v.Value().([]byte)
	if !ok {
		t.Fatalf("InvocationID value type = %T, want []byte", v.Value())
	}
	if len(b) != 0 {
		t.Errorf("InvocationID byte slice should be zero-length sentinel, got %d bytes", len(b))
	}
}

// TestPropertyStubGetAllEmpty covers the GetAll path used by tools
// like `busctl introspect <path>` that enumerate every property.
func TestPropertyStubGetAllEmpty(t *testing.T) {
	p := newPropertyStub()
	got, dErr := p.GetAll("org.freedesktop.systemd1.Unit")
	if dErr != nil {
		t.Fatal(dErr)
	}
	if len(got) != 0 {
		t.Errorf("GetAll = %v, want empty", got)
	}
}

// TestPropertyStubSetIsNoop ensures writes never raise; some callers
// optimistically set properties after StartTransientUnit and would
// abort on error.
func TestPropertyStubSetIsNoop(t *testing.T) {
	p := newPropertyStub()
	if err := p.Set("any.iface", "AnyProp", dbus.MakeVariant("x")); err != nil {
		t.Errorf("Set returned error: %v", err)
	}
}
