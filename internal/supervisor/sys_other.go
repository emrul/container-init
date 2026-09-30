//go:build !linux

package supervisor

import (
	"syscall"
)

// Stubs for non-Linux build hosts: container-init runs only on Linux,
// and these let `go build ./...` and `go test ./...` work elsewhere.

func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func applyCredential(attr *syscall.SysProcAttr, uid, gid uint32, groups []uint32) {
}

func spawnIntoCgroup(attr *syscall.SysProcAttr, fd int) {}

func killGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}

// processRunning cannot tell a zombie apart without /proc; it reports
// whether pid exists.
func processRunning(pid int) bool { return processAlive(pid) }
