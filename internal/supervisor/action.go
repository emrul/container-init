package supervisor

import (
	"errors"
	"log"

	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// Unit actions: FailureAction=, SuccessAction= and ExitContainerOnFailure=
// taking the container down. As in systemd, FailureAction= fires once a
// unit is failed and SuccessAction= once it is inactive after a clean
// exit -- both only when Restart= will not start it again, and never
// for a unit stopped by reverse shutdown. ExitContainerOnFailure= is
// container-init's own: it fires on a unit's first failure, before
// Restart= is considered.

// exitRequest is the unit action taking the container down. The first
// one wins; shutdown reads it once Stop has been called.
type exitRequest struct {
	unit   string
	action string // the directive that fired, e.g. "FailureAction=exit"
	result string // the unit's result when it fired
	force  bool   // exit-force: kill every unit at once
	// status is the container's exit status, or -1 for shutdown's own
	// (0, or 1 if a unit had to be killed).
	status int
}

// unitSucceeded handles a service that exited cleanly and will not be
// started again. Unless it is a RemainAfterExit= oneshot, which stays
// active, it is now inactive, and its SuccessAction= fires.
func (s *Supervisor) unitSucceeded(u *unit.Unit) {
	if u.Type == unit.TypeOneshot && u.RemainAfterExit {
		return
	}
	s.runAction(u, "SuccessAction", u.SuccessAction,
		statusFor(u.SuccessActionExitStatusSet, u.SuccessActionExitStatus, s.mainStatus(u, nil)))
}

// failureAction fires u's FailureAction=, u having failed for good.
// main is the exit code of its main process, or -1 if it has none.
func (s *Supervisor) failureAction(u *unit.Unit, main int) {
	s.runAction(u, "FailureAction", u.FailureAction,
		statusFor(u.FailureActionExitStatusSet, u.FailureActionExitStatus, main))
}

// exitOnFailure takes the container down for u's
// ExitContainerOnFailure=. FailureActionExitStatus= gives its status,
// as it does FailureAction='s, and FailureAction=exit-force makes it
// forced.
func (s *Supervisor) exitOnFailure(u *unit.Unit, exitErr error) {
	a := unit.ActionExit
	if u.FailureAction == unit.ActionExitForce {
		a = unit.ActionExitForce
	}
	s.requestExit(u, "ExitContainerOnFailure=", a,
		statusFor(u.FailureActionExitStatusSet, u.FailureActionExitStatus, s.mainStatus(u, exitErr)))
}

func (s *Supervisor) runAction(u *unit.Unit, directive string, a unit.EmergencyAction, status int) {
	if a == unit.ActionNone || s.stopping() {
		return
	}
	s.requestExit(u, directive+"="+a.String(), a, status)
}

// requestExit records why the container is going down, unless an
// earlier request did, and begins reverse shutdown. The request is set
// before Stop, so shutdown always finds it.
func (s *Supervisor) requestExit(u *unit.Unit, action string, a unit.EmergencyAction, status int) {
	req := &exitRequest{unit: u.Name, action: action, force: a == unit.ActionExitForce, status: status}
	s.mu.Lock()
	if st := s.status[u.Name]; st != nil {
		req.result = st.Result
	}
	s.mu.Unlock()
	if s.exitReq.CompareAndSwap(nil, req) {
		log.Printf("unit %s: %s -- initiating reverse shutdown", u.Name, action)
		fields := map[string]any{"action": action}
		if status >= 0 {
			fields["status"] = status
		}
		s.event(u.Name, "exit_requested", fields)
	}
	s.Stop()
}

// stateExit is the state file's pid1.exit: nil until a unit action is
// taking the container down.
func (s *Supervisor) stateExit() *statefile.Exit {
	req := s.exitReq.Load()
	if req == nil {
		return nil
	}
	e := &statefile.Exit{Unit: req.unit, Action: req.action, Result: req.result}
	if req.status >= 0 {
		status := req.status
		e.Status = &status
	}
	return e
}

// mainStatus returns the exit code of u's last run, as systemd
// propagates the main process's: -1 when it did not start or was
// killed by a signal, since there is no exit code to pass on.
func (s *Supervisor) mainStatus(u *unit.Unit, exitErr error) int {
	var se *startError
	if errors.As(exitErr, &se) || errors.Is(exitErr, errStopping) {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.services[u.Name]
	if st == nil || !st.exited || st.lastExit.Signaled {
		return -1
	}
	return st.lastExit.ExitCode
}

// statusFor is a unit's configured exit status if set, else main.
func statusFor(set bool, configured, main int) int {
	if set {
		return configured
	}
	return main
}
