package socketact

import (
	"fmt"
	"os/exec"
	"strings"
)

// PrepareNative configures cmd to be exec'd with b.File inherited as
// fd 3, plus the sd_listen_fds env (LISTEN_FDS, LISTEN_PID,
// LISTEN_FDNAMES). LISTEN_PID must equal the consumer's getpid() at
// the moment it reads the env, which we cannot know before exec; the
// portable fix is to wrap the original argv in `sh -c 'export
// LISTEN_PID=$$; exec <argv>'` so the assignment fires inside the
// final process image. The shim is two extra syscalls (fork+exec) on
// the activation path, which is dwarfed by helper cold-start.
//
// The bound listener stays open in container-init so the same fd can
// be re-passed across service restarts.
func PrepareNative(cmd *exec.Cmd, b *Bound) error {
	if b == nil || b.File == nil {
		return fmt.Errorf("native activation: nil bound listener")
	}
	if len(cmd.Args) == 0 {
		return fmt.Errorf("native activation: empty argv")
	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, b.File)

	cmd.Env = append(stripListenEnv(cmd.Env),
		"LISTEN_FDS=1",
		"LISTEN_FDNAMES="+b.Listener.Raw,
	)

	wrapped := "export LISTEN_PID=$$; exec " + shellQuoteArgv(cmd.Args)
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"sh", "-c", wrapped}
	return nil
}

// stripListenEnv removes any LISTEN_* entry so we re-add a single
// canonical pair without duplicates.
func stripListenEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "LISTEN_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// shellQuoteArgv renders argv as a single sh-safe command line.
// Single-quoting is sufficient for our use; embedded single quotes
// aren't supported (we don't expect them in production unit values).
func shellQuoteArgv(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", "'\\''"))
		b.WriteByte('\'')
	}
	return b.String()
}
