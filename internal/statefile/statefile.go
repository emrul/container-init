// Package statefile is the format of the unit-state file PID 1 writes
// with --state-file and `container-init health` reads (see
// docs/design/state-file.md). It holds only the format; the supervisor
// decides what goes in it.
package statefile

import (
	"encoding/json"
	"fmt"
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
