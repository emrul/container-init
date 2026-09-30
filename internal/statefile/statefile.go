// Package statefile is the format of the unit-state file PID 1 writes
// with --state-file and `container-init health` reads (see
// docs/design/state-file.md). It holds only the format; the supervisor
// decides what goes in it.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
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

// Write replaces the file at path with f, atomically: it writes an
// exclusive temporary file in path's directory, gives it mode 0644
// through the open file (never by path), and renames it over path. A
// missing directory is created, with any missing parents, 0755; an
// existing one is left as it is. created reports the directories it
// made.
func Write(path string, f *File) (created []string, err error) {
	dir := filepath.Dir(path)
	created, err = mkdirs(dir)
	if err != nil {
		return created, err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return created, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return created, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return created, err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return created, err
	}
	if err = tmp.Close(); err != nil {
		return created, err
	}
	return created, os.Rename(tmp.Name(), path)
}

// mkdirs creates dir and any missing parents 0755, whatever the umask,
// and never touches a directory that already exists.
func mkdirs(dir string) (created []string, err error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := os.Mkdir(d, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // made meanwhile by someone else: theirs
			}
			return created, err
		}
		created = append(created, d)
		// The umask may have narrowed it; this directory is ours.
		if err := os.Chmod(d, 0o755); err != nil {
			return created, err
		}
	}
	return created, nil
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
