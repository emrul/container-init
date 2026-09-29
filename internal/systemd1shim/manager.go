package systemd1shim

import (
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// managerPath is the well-known object path real systemd exports
// org.freedesktop.systemd1.Manager on.
const managerPath dbus.ObjectPath = "/org/freedesktop/systemd1"

// managerIface is the D-Bus interface name clients call.
const managerIface = "org.freedesktop.systemd1.Manager"

// property mirrors the (sv) wire shape of a systemd transient-unit
// property. We accept the full vector, ignore every field, and only
// log enough to make debugging easy.
type property struct {
	Name  string
	Value dbus.Variant
}

// auxUnit mirrors the (sa(sv)) shape that StartTransientUnit's `aux`
// array uses for sibling units (e.g. a .scope whose enclosing .slice
// is sent alongside).
type auxUnit struct {
	Name       string
	Properties []property
}

// signalEmitter decouples the manager from a real D-Bus connection so
// tests can capture emitted signals without spinning up a bus daemon.
type signalEmitter interface {
	Emit(path dbus.ObjectPath, name string, values ...any) error
}

// logger is the optional callback the shim uses to surface activity.
// nil-safe.
type logger func(format string, args ...any)

type manager struct {
	emitter signalEmitter
	log     logger
	nextJob atomic.Uint32 // monotonic job id; 0 reserved
	units   *unitSet      // units started through the shim
}

func newManager(emitter signalEmitter, log logger) *manager {
	return &manager{emitter: emitter, log: log, units: newUnitSet(maxUnits)}
}

func (m *manager) logf(format string, args ...any) {
	if m.log != nil {
		m.log(format, args...)
	}
}

// StartTransientUnit is the only method clients meaningfully care
// about. We synthesize a job, fire JobNew + JobRemoved("done"), and
// return the synthesized job path. systemd-run blocks on JobRemoved
// before exec'ing its target argv, so the signal order matters.
func (m *manager) StartTransientUnit(name, mode string, properties []property, aux []auxUnit) (dbus.ObjectPath, *dbus.Error) {
	id := m.nextJob.Add(1)
	jobPath := dbus.ObjectPath("/org/freedesktop/systemd1/job/" + uint32ToA(id))
	m.logf("StartTransientUnit: unit=%q mode=%q props=%d aux=%d -> %s", name, mode, len(properties), len(aux), jobPath)
	// Record the unit before JobRemoved goes out: a client may read
	// its properties as soon as it sees the job finish.
	m.units.add(name)

	// JobNew and JobRemoved are fire-and-forget; if the emitter is
	// disconnected mid-call the client will time out, which is the
	// honest failure mode -- we don't try to paper over a broken bus.
	if err := m.emitter.Emit(managerPath, managerIface+".JobNew", id, jobPath, name); err != nil {
		m.logf("JobNew emit failed: %v", err)
	}
	if err := m.emitter.Emit(managerPath, managerIface+".JobRemoved", id, jobPath, name, "done"); err != nil {
		m.logf("JobRemoved emit failed: %v", err)
	}
	return jobPath, nil
}

// StartUnit / StopUnit / ReloadUnit / RestartUnit: clients that aren't
// systemd-run sometimes call these. Same shape: synthesize a job,
// emit JobRemoved, return path. Start and Restart record the unit as
// active; Stop forgets it.
func (m *manager) StartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	m.units.add(name)
	return m.synthesizeJob("StartUnit", name, mode)
}

func (m *manager) StopUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	m.units.remove(name)
	return m.synthesizeJob("StopUnit", name, mode)
}

func (m *manager) ReloadUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	return m.synthesizeJob("ReloadUnit", name, mode)
}

func (m *manager) RestartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	m.units.add(name)
	return m.synthesizeJob("RestartUnit", name, mode)
}

func (m *manager) synthesizeJob(op, name, mode string) (dbus.ObjectPath, *dbus.Error) {
	id := m.nextJob.Add(1)
	jobPath := dbus.ObjectPath("/org/freedesktop/systemd1/job/" + uint32ToA(id))
	m.logf("%s: unit=%q mode=%q -> %s", op, name, mode, jobPath)
	if err := m.emitter.Emit(managerPath, managerIface+".JobNew", id, jobPath, name); err != nil {
		m.logf("JobNew emit failed: %v", err)
	}
	if err := m.emitter.Emit(managerPath, managerIface+".JobRemoved", id, jobPath, name, "done"); err != nil {
		m.logf("JobRemoved emit failed: %v", err)
	}
	return jobPath, nil
}

// GetUnit returns a deterministic object path. Real systemd would
// return the existing unit's path or NoSuchUnit; we return a derived
// path for any name because callers that follow up with property
// reads get an empty result rather than a hard error. Units the shim
// started answer those reads as active (see propertyStub).
func (m *manager) GetUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	return unitPath(name), nil
}

// ListUnits returns an empty list. Real systemd returns
// a(ssssssouso); an empty array satisfies the wire signature without
// inventing fake units.
func (m *manager) ListUnits() ([]struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Following   string
	UnitPath    dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}, *dbus.Error) {
	return nil, nil
}

// Subscribe / Unsubscribe / Reload / Reexecute are common no-arg
// calls. Returning nil error is sufficient.
func (m *manager) Subscribe() *dbus.Error   { return nil }
func (m *manager) Unsubscribe() *dbus.Error { return nil }
func (m *manager) Reload() *dbus.Error      { return nil }
func (m *manager) Reexecute() *dbus.Error   { return nil }

// uint32ToA renders a uint32 without importing strconv (kept to avoid
// pulling strconv into a hot path; the values are small).
func uint32ToA(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// escapeUnitName mirrors systemd's bus-path escaping: every char that
// isn't [A-Za-z0-9_] becomes _XX (hex). Good enough for GetUnit's
// return value; we never round-trip the result.
func escapeUnitName(name string) string {
	out := make([]byte, 0, len(name))
	const hex = "0123456789abcdef"
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_', hex[c>>4], hex[c&0x0f])
		}
	}
	return string(out)
}
