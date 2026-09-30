//go:build !linux

package supervisor

import (
	"syscall"
)

// Stub implementations for non-linux build hosts. The container-init
// binary only ever runs on linux; these exist solely so `go build
// ./...` and `go test ./...` succeed during local development.

func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func applyCredential(attr *syscall.SysProcAttr, uid, gid uint32, groups []uint32) {
	// Linux-only path. Stub for `go test ./...` from a darwin host.
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
