// Package systemd1shim answers the org.freedesktop.systemd1 D-Bus API
// well enough to satisfy clients that probe for systemd by spawning
// systemd-run (notably Ptyxis, which wraps every shell with
// `systemd-run --user --scope`). The shim is a *compatibility surface*:
// it returns success without creating cgroups, enforcing resource
// limits, or persisting unit state. Properties like Slice=,
// MemoryMax=, CPUQuota= are accepted and ignored.
//
// The shim opts in via main.go's --systemd1-shim flag. Address forms:
//
//	system           the system bus
//	user             the calling uid's user bus ($XDG_RUNTIME_DIR/bus)
//	user:UID         that uid's user bus (/run/user/UID/bus)
//	unix:path=…      any D-Bus address string godbus understands
//
// Multiple addresses can be comma-separated; the shim races a goroutine
// per address that dials with retry, RequestName's
// org.freedesktop.systemd1, and reconnects on disconnect.
//
// If a real systemd is already on the bus the RequestName loses and
// the shim logs once and gives up that bus -- without disrupting the
// real systemd.
package systemd1shim
