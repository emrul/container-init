//go:build !linux

package supervisor

import (
	"errors"
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

func killGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

func waitReadable(fd int, stop <-chan struct{}) error {
	return errors.New("waitReadable: not implemented on this platform (linux-only build)")
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}
