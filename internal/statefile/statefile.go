// Package statefile is the format of the unit-state file PID 1 writes
// with --state-file and `container-init health` reads (see
// docs/design/state-file.md). It holds only the format; the supervisor
// decides what goes in it.
package statefile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Version is the file format this package writes and reads.
const Version = 1

// File is the whole state file.
type File struct {
	Version int             `json:"version"`
	Written Time            `json:"written"`
	PID1    PID1            `json:"pid1"`
	Units   map[string]Unit `json:"units"`
}

// PID1 describes container-init itself.
type PID1 struct {
	Version  string `json:"version"`
	Started  Time   `json:"started"`
	Stopping bool   `json:"stopping"`
}

// Unit is one unit's state. The words are systemd's where it has them.
type Unit struct {
	Type     string `json:"type"`   // simple, oneshot, forking, socket
	Active   string `json:"active"` // inactive, activating, active, deactivating, failed
	Sub      string `json:"sub"`
	Result   string `json:"result"`
	Runs     int    `json:"runs"`
	Restarts int    `json:"restarts"`
	Since    Time   `json:"since"`
	// Never, on a unit that will not start this boot: condition,
	// dependency or missing-requirement.
	Never string `json:"never,omitempty"`
	// Activation, on a unit something else starts: socket or on-failure.
	Activation string `json:"activation,omitempty"`
	// Service, on every socket: the service it activates.
	Service string `json:"service,omitempty"`
	// Sockets, on a socket-activated service: every loaded socket whose
	// Service names it, sorted.
	Sockets []string `json:"sockets,omitempty"`
}

// Values of Unit fields.
const (
	ActiveInactive     = "inactive"
	ActiveActivating   = "activating"
	ActiveActive       = "active"
	ActiveDeactivating = "deactivating"
	ActiveFailed       = "failed"

	SubDead        = "dead"
	SubStart       = "start"
	SubRunning     = "running"
	SubExited      = "exited"
	SubAutoRestart = "auto-restart"
	SubStop        = "stop"
	SubFailed      = "failed"
	SubListening   = "listening"

	ResultSuccess              = "success"
	ResultExitCode             = "exit-code"
	ResultSignal               = "signal"
	ResultStartLimitHit        = "start-limit-hit"
	ResultResources            = "resources"
	ResultTriggerLimitHit      = "trigger-limit-hit"
	ResultServiceStartLimitHit = "service-start-limit-hit"
	ResultDependency           = "dependency"
	NeverCondition             = "condition"
	NeverDependency            = "dependency"
	NeverMissingRequirement    = "missing-requirement"
	ActivationSocket           = "socket"
	ActivationOnFailure        = "on-failure"
	TypeSocket                 = "socket"
)

// Time is a timestamp written as RFC 3339, UTC, whole seconds.
type Time struct{ time.Time }

const timeLayout = "2006-01-02T15:04:05Z"

// MarshalJSON writes t in UTC, truncated to the second.
func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Truncate(time.Second).Format(timeLayout))
}

// UnmarshalJSON reads an RFC 3339 timestamp.
func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("timestamp %q: %w", s, err)
	}
	t.Time = v
	return nil
}

// Result says what Write did besides writing the file.
type Result struct {
	// Created lists the directories it made.
	Created []string
	// Unwidened lists those it made beneath a directory another uid can
	// write, and so left at mkdir's mode, the umask applied, rather
	// than set to 0755: someone who can replace the name could have
	// put a directory of theirs there, and it is not chmod'ed.
	Unwidened []string
}

// Write replaces the file at path with f, atomically. Everything past
// the directories that already exist is done through directory
// descriptors, never by path (see openDir), and the file is an
// exclusive temporary one made in the opened directory, given mode
// 0644 through its own descriptor, written and closed -- a failure at
// any step, the close's included, leaves the previous file in place --
// then renamed over path within that directory.
func Write(path string, f *File) (res Result, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return res, err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return res, err
	}
	data = append(data, '\n')
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	dfd, res, err := openDir(dir)
	if err != nil {
		return res, err
	}
	defer unix.Close(dfd)

	name, fd, err := createTemp(dfd, base)
	if err != nil {
		return res, fmt.Errorf("%s: %w", dir, err)
	}
	tmp := os.NewFile(uintptr(fd), filepath.Join(dir, name))
	err = tmp.Chmod(0o644)
	if err == nil {
		_, err = tmp.Write(data)
	}
	// A filesystem can report a failed write only here: check it
	// before publishing.
	if cerr := closeTemp(tmp); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(dfd, name, dfd, base)
	}
	if err != nil {
		_ = unix.Unlinkat(dfd, name, 0)
		return res, fmt.Errorf("write %s: %w", abs, err)
	}
	return res, nil
}

// closeTemp closes the written temporary file; a variable so tests can
// fail it, as a filesystem reporting a write error on close would.
var closeTemp = (*os.File).Close

// createTemp creates an exclusive, not-yet-visible file beside base in
// the directory dfd, never following a symlink planted at its name.
func createTemp(dfd int, base string) (string, int, error) {
	for range 100 {
		var b [6]byte
		_, _ = rand.Read(b[:])
		name := "." + base + "." + hex.EncodeToString(b[:])
		fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return name, fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", -1, err
		}
	}
	return "", -1, errors.New("no unused temporary name")
}

// openDir opens dir, creating it and any missing parents. The
// directories that already exist are opened as they are, symlinks and
// all: they are the image's layout. From the first one it creates on,
// each is made with mkdirat in the directory opened before it and
// opened with O_NOFOLLOW, and must be a directory owned by this
// process. Its mode is set to 0755, through its descriptor, only when
// that parent is trusted -- nobody else can rename or replace entries
// in it -- so the directory opened is certainly the one just made.
// Beneath any other parent it is left at mkdir's own mode, and nothing
// is chmod'ed. A directory it did not make (another process made it
// meanwhile) is used as it is.
func openDir(dir string) (fd int, res Result, err error) {
	fd, err = unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, res, err
	}
	at := "/"
	for _, name := range strings.Split(strings.TrimPrefix(filepath.Clean(dir), "/"), "/") {
		if name == "" {
			continue
		}
		next, err := openChild(fd, at, name)
		if errors.Is(err, unix.ENOENT) {
			var made, widened bool
			next, made, widened, err = makeChild(fd, at, name)
			if made {
				path := filepath.Join(at, name)
				res.Created = append(res.Created, path)
				if !widened {
					res.Unwidened = append(res.Unwidened, path)
				}
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, res, err
		}
		fd, at = next, filepath.Join(at, name)
	}
	return fd, res, nil
}

// trusted reports whether nobody but this process (or root) can rename
// or replace entries in the directory dfd: it is owned by us or root
// and writable by no one else, or it is sticky, which keeps others
// from replacing an entry they do not own.
func trusted(dfd int) (bool, error) {
	var st unix.Stat_t
	if err := unix.Fstat(dfd, &st); err != nil {
		return false, err
	}
	if int(st.Uid) != os.Geteuid() && st.Uid != 0 {
		return false, nil
	}
	return st.Mode&0o022 == 0 || st.Mode&unix.S_ISVTX != 0, nil
}

// afterMkdir, when set by a test, runs between a directory's mkdirat
// and its open.
var afterMkdir func(parent, name string)

func openChild(parent int, at, name string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		err = fmt.Errorf("open %s: %w", filepath.Join(at, name), err)
	}
	return fd, err
}

// makeChild creates name in parent and opens the directory it made.
// made is false when something else took the name first; that is then
// opened as an existing directory. widened says its mode was set to
// 0755; see openDir.
func makeChild(parent int, at, name string) (fd int, made, widened bool, err error) {
	path := filepath.Join(at, name)
	safe, err := trusted(parent)
	if err != nil {
		return -1, false, false, err
	}
	mode := uint32(0o755) // the umask applies: nothing widens it later
	if safe {
		mode = 0o700 // nothing reachable through it until the fchmod
	}
	if err := unix.Mkdirat(parent, name, mode); err != nil {
		if errors.Is(err, unix.EEXIST) {
			fd, err := openChild(parent, at, name)
			return fd, false, false, err
		}
		return -1, false, false, fmt.Errorf("mkdir %s: %w", path, err)
	}
	if afterMkdir != nil {
		afterMkdir(at, name)
	}
	fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, true, false, fmt.Errorf("open %s after creating it (replaced?): %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, true, false, err
	}
	if int(st.Uid) != os.Geteuid() || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return -1, true, false, fmt.Errorf("%s was replaced while being created", path)
	}
	if !safe {
		return fd, true, false, nil
	}
	if err := unix.Fchmod(fd, 0o755); err != nil {
		unix.Close(fd)
		return -1, true, false, err
	}
	return fd, true, true, nil
}

// Read reads and checks the file at path. A version it does not know
// is an error.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("%s: unknown version %d (this container-init reads %d)", path, f.Version, Version)
	}
	return &f, nil
}
