// Package health is `container-init health`: it reads the state file
// PID 1 writes with --state-file and says whether the container is
// healthy, for use as, or inside, a Docker HEALTHCHECK (see
// docs/design/state-file.md). Which units matter is the image's
// policy, given with --require; the file only reports.
package health

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/emrul/container-init/internal/statefile"
)

// DefaultMaxAge is three missed heartbeats.
const DefaultMaxAge = 30 * time.Second

// Main runs the subcommand with args (after "health"), printing one
// line to out, and returns the exit code: 0 healthy, 1 unhealthy. It
// never returns 2, which Docker reserves; a usage error is unhealthy.
func Main(args []string, out io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("container-init health", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("state-file", "", "the state file PID 1 writes (required)")
	var require requireList
	fs.Var(&require, "require", "comma-separated units that must be healthy; repeatable (default: no unit may be failed)")
	maxAge := fs.Duration("max-age", DefaultMaxAge, "how old the file's heartbeat may be")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *path == "" || fs.NArg() > 0 {
		fmt.Fprintln(out, "unhealthy: usage: container-init health --state-file <path> [--require <unit>,...] [--max-age 30s]")
		return 1
	}
	f, err := statefile.Read(*path)
	if err != nil {
		fmt.Fprintf(out, "unhealthy: %v\n", err)
		return 1
	}
	ok, why := Check(f, now, require, *maxAge)
	if ok {
		fmt.Fprintf(out, "healthy: %s\n", why)
		return 0
	}
	fmt.Fprintf(out, "unhealthy: %s\n", why)
	return 1
}

type requireList []string

func (r *requireList) String() string { return strings.Join(*r, ",") }

func (r *requireList) Set(v string) error {
	for _, name := range strings.Split(v, ",") {
		if name = strings.TrimSpace(name); name != "" {
			*r = append(*r, name)
		}
	}
	return nil
}

// Check says whether f, read at now, reports a healthy container, and
// why. With require, only the named units are judged, each by the rule
// for its kind; without it, no unit may be failed.
func Check(f *statefile.File, now time.Time, require []string, maxAge time.Duration) (bool, string) {
	if age := now.Sub(f.Written.Time); age > maxAge {
		return false, fmt.Sprintf("heartbeat stale: written %s ago (max %s)", age.Truncate(time.Second), maxAge)
	}
	if f.PID1.Stopping {
		return false, "container-init is shutting down"
	}
	if len(require) == 0 {
		var failed []string
		for name, u := range f.Units {
			if u.Active == statefile.ActiveFailed {
				failed = append(failed, name)
			}
		}
		if len(failed) > 0 {
			slices.Sort(failed)
			return false, "failed: " + strings.Join(failed, ", ")
		}
		return true, fmt.Sprintf("%d units, none failed", len(f.Units))
	}
	for _, name := range require {
		if why := judge(f, name); why != "" {
			return false, why
		}
	}
	return true, fmt.Sprintf("%d required units pass", len(require))
}

// judge returns why the required unit name fails, or "".
func judge(f *statefile.File, name string) string {
	u, ok := f.Units[name]
	if !ok {
		return name + " is not loaded"
	}
	if u.Type == statefile.TypeSocket {
		if u.Active != statefile.ActiveActive {
			return fmt.Sprintf("%s is %s", name, describe(u))
		}
		svc, ok := f.Units[u.Service]
		if !ok {
			return fmt.Sprintf("%s activates %s, which is not loaded", name, u.Service)
		}
		if why := judgeService(f, u.Service, svc); why != "" {
			return fmt.Sprintf("%s's service: %s", name, why)
		}
		return ""
	}
	return judgeService(f, name, u)
}

func judgeService(f *statefile.File, name string, u statefile.Unit) string {
	fail := func() string { return fmt.Sprintf("%s is %s", name, describe(u)) }
	if u.Never != "" || u.Active == statefile.ActiveFailed || u.Active == statefile.ActiveActivating ||
		u.Active == statefile.ActiveDeactivating {
		return fail()
	}
	finished := u.Active == statefile.ActiveActive && u.Sub == statefile.SubExited ||
		u.Active == statefile.ActiveInactive && u.Result == statefile.ResultSuccess && u.Runs >= 1
	switch {
	case u.Activation == statefile.ActivationSocket:
		// Up or waiting for a connection, and every socket listening:
		// the rule follows the recorded sockets, never unit names.
		for _, s := range u.Sockets {
			if sock := f.Units[s]; sock.Active != statefile.ActiveActive {
				return fmt.Sprintf("%s's socket %s is %s", name, s, describe(sock))
			}
		}
		return ""
	case u.Activation == statefile.ActivationOnFailure:
		// Not triggered, or triggered and finished.
		if u.Active == statefile.ActiveInactive && u.Runs == 0 || finished {
			return ""
		}
		return fail()
	case u.Type == "oneshot":
		if finished {
			return ""
		}
		if u.Runs == 0 {
			return name + " has not run"
		}
		return fail()
	default:
		if u.Active == statefile.ActiveActive {
			return ""
		}
		return fail()
	}
}

// describe renders u's state for a message.
func describe(u statefile.Unit) string {
	if u.Active == "" {
		return "not loaded"
	}
	s := u.Active + "/" + u.Sub
	if u.Result != statefile.ResultSuccess {
		s += " (" + u.Result + ")"
	}
	if u.Never != "" {
		s += ", never starting: " + u.Never
	}
	return s
}
