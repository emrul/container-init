//go:build linux

package supervisor

import (
	"errors"
	"syscall"
)

// procAttr returns the SysProcAttr that puts every spawned service
// into its own process group, so reverse shutdown can signal the
// whole tree at once with a negative PID.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// applyCredential mutates attr in place to add a Credential block
// driving setresuid/setresgid before exec. Pass an empty groups slice
// to drop supplementary groups entirely (NoSetGroups=false +
// Groups=nil yields setgroups([])); pass non-empty to set them.
func applyCredential(attr *syscall.SysProcAttr, uid, gid uint32, groups []uint32) {
	cred := &syscall.Credential{
		Uid:    uid,
		Gid:    gid,
		Groups: groups,
	}
	attr.Credential = cred
}

// killGroup signals every process in pid's group.
func killGroup(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		// Refuse to send to PG ID <=1: -1 means "all processes",
		// 0 means "current PG" (container-init itself), and 1 is
		// our own PID. Any of these would kill the supervisor.
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}

// waitReadable blocks until fd is readable or stop is closed. Uses
// syscall.Select; the contract on EINTR is that the FdSet is left
// unmodified (Linux), so we MUST gate readability on the return
// count rather than the FdSet bits — Go runtime preemption (SIGURG
// since 1.14) interrupts select() routinely, and treating
// "EINTR + bit-still-set" as readable produces phantom first-connect
// events on every busy goroutine.
func waitReadable(fd int, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return errors.New("stopped")
		default:
		}
		var rfds syscall.FdSet
		fdSet(&rfds, fd)
		tv := syscall.Timeval{Sec: 0, Usec: 100_000}
		n, err := syscall.Select(fd+1, &rfds, nil, nil, &tv)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		if n > 0 && fdIsSet(&rfds, fd) {
			return nil
		}
	}
}

func fdSet(p *syscall.FdSet, fd int) {
	p.Bits[fd/64] |= 1 << (uint(fd) % 64)
}
func fdIsSet(p *syscall.FdSet, fd int) bool {
	return p.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
}

// processAlive returns true when pid has not been reaped. Uses the
// kill(pid, 0) probe — ESRCH means gone, anything else (including 0
// and EPERM) means present.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}
