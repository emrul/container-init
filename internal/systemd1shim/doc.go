// Package systemd1shim answers the org.freedesktop.systemd1 D-Bus API
// well enough to satisfy clients that probe for systemd by spawning
// systemd-run (notably Ptyxis, which wraps every shell with
// `systemd-run --user --scope`). The shim is a *compatibility surface*:
// it returns success without creating cgroups, enforcing resource
// limits, or persisting unit state. Properties like Slice=,
// MemoryMax=, CPUQuota= are accepted and ignored.
//
// Two ways to run it:
//
//  1. Built into PID 1 via container-init's --systemd1-shim flag.
//     Suitable for the system bus or any bus a root process is allowed
//     to own (most session-bus daemons reject EXTERNAL-auth from a
//     non-owner uid, so this path doesn't help with user sessions).
//
//  2. The standalone cmd/systemd1-shim binary, supervised by
//     container-init as a normal unit with User= set to the bus
//     owner. This is the right shape for desktop session buses.
//
// Address forms accepted by both entry points:
//
//	system                 the system bus
//	user                   the calling uid's user bus ($XDG_RUNTIME_DIR/bus)
//	user:UID               that uid's user bus (/run/user/UID/bus)
//	user:env:VAR           /run/user/$VAR/bus (e.g. user:env:KASM_OS_UID)
//	dbus:env-file:PATH     read DBUS_SESSION_BUS_ADDRESS from PATH
//	unix:path=…            any raw D-Bus address string
//
// Multiple addresses can be comma-separated; the shim races a goroutine
// per address that dials with retry, RequestName's
// org.freedesktop.systemd1, and reconnects on disconnect.
//
// If a real systemd is already on the bus the RequestName loses and
// the shim logs once and gives up that bus -- without disrupting the
// real systemd.
package systemd1shim
