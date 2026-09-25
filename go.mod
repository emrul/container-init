module github.com/emrul/container-init

go 1.26.0

toolchain go1.27.1

require (
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/godbus/dbus/v5 v5.2.2
)

require golang.org/x/sys v0.48.0 // indirect

// In-tree patched godbus. The patch in third_party/godbus/decoder.go
// fixes a reflect.Append type-mismatch panic that hits any time a
// variant value carries `ah` (array of unix-fd indices) AND the
// message brings real fds -- godbus's `case 'h'` returns UnixFD
// when fds are attached but typeFor("h") is UnixFDIndex, so the
// slice append panics. Recovered into "Invalid type / number of
// args", so callers see InvalidArgs before the handler runs.
//
// Repro: systemd-run --user --scope on systemd 255 sends PIDFDs(ah)
// with one fd, hitting the bug on every call. Drop this replace
// once upstream merges the equivalent fix. See
// third_party/godbus/PATCHES.md for the diff and rationale.
replace github.com/godbus/dbus/v5 => ./third_party/godbus
