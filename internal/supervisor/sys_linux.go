//go:build linux

package supervisor

import (
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

// spawnIntoCgroup makes the child start life inside the cgroup open
// as fd: Go then forks with clone3(CLONE_INTO_CGROUP), so the child
// runs no instruction, and forks no child, outside it. fd < 0 clears
// the request.
func spawnIntoCgroup(attr *syscall.SysProcAttr, fd int) {
	attr.UseCgroupFD = fd >= 0
	attr.CgroupFD = max(fd, 0)
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

// processAlive returns true when pid has not been reaped. Uses the
// kill(pid, 0) probe -- ESRCH means gone, anything else (including 0
// and EPERM) means present.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}
