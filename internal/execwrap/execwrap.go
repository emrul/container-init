// Package execwrap is the first program every service spawn runs.
//
// The supervisor spawns services under the pid1 dispatcher's lock, and
// cmd.Start does not return until the child has exec'd or failed, so
// everything the child does between fork and exec happens while the
// lock is held and the reaper is stalled. Left to os/exec, that
// includes chdir(WorkingDirectory=) and the execve of ExecStart=, which
// can block for as long as a hung FUSE or NFS mount likes.
//
// So the supervisor instead execs container-init's own binary
// (/proc/self/exe) from "/", with this package's argument, and this
// wrapper does the chdir and the final execve after the lock is
// released. The only work left between fork and exec is kernel work on
// objects PID 1 owns. The wrapper reports a failure on a status pipe
// that the final execve closes (close-on-exec), so the supervisor can
// tell "could not start" from the service's own exit status.
package execwrap

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// Arg is os.Args[1] when a process should act as the wrapper.
const Arg = "__exec"

// Exit statuses when the wrapper fails, systemd's values from
// systemd.exec(5) "Process Exit Codes".
const (
	ExitChdir = 200
	ExitExec  = 203
)

// Argv returns the wrapper's argv. After the spawn, the wrapper changes
// to dir (when non-empty) and execs path with argv. statusFD is the
// descriptor number, in the child, of the write end of the status pipe.
func Argv(statusFD int, dir, path string, argv []string) []string {
	return append([]string{"container-init", Arg, strconv.Itoa(statusFD), dir, path, "--"}, argv...)
}

// Main runs the wrapper and does not return when os.Args[1] is Arg.
// Otherwise it returns at once. Call it first thing in main, and in
// TestMain of any package whose tests spawn services.
func Main() {
	if len(os.Args) < 2 || os.Args[1] != Arg {
		return
	}
	a := os.Args[2:]
	if len(a) < 5 || a[3] != "--" {
		fmt.Fprintf(os.Stderr, "container-init %s: bad arguments %q\n", Arg, a)
		os.Exit(ExitExec)
	}
	statusFD, err := strconv.Atoi(a[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "container-init %s: bad status fd %q\n", Arg, a[0])
		os.Exit(ExitExec)
	}
	dir, path, argv := a[1], a[2], a[4:]
	syscall.CloseOnExec(statusFD)

	if dir != "" {
		if err := syscall.Chdir(dir); err != nil {
			fail(statusFD, ExitChdir, fmt.Sprintf("chdir %s: %v", dir, err))
		}
	}
	err = syscall.Exec(path, argv, os.Environ())
	fail(statusFD, ExitExec, fmt.Sprintf("exec %s: %v", path, err))
}

func fail(statusFD, code int, msg string) {
	_, _ = syscall.Write(statusFD, []byte(msg))
	os.Exit(code)
}
