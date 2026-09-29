package supervisor

import (
	"errors"
	"sort"
	"time"

	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// Unit state, as the state file reports it (docs/design/state-file.md).
// Each unit's statefile.Unit lives in s.status, guarded by s.mu, and is
// changed only through the helpers here, at the points in the lifecycle
// where the supervisor itself decides what happens next. A change sets
// Since when active/sub move, and signals s.changed without blocking:
// whoever writes the file takes its own snapshot, so no lifecycle path
// ever waits on it.

// initStatus builds every unit's starting state: inactive/dead, not yet
// run, since started. A socket records the service it activates; a
// service records every loaded socket that names it. What Run's start
// plan decides is known already, and recorded now so the first report
// is complete: a skipped unit will never start, and a unit a socket or
// OnFailure= starts says so.
func initStatus(units []*unit.Unit, started time.Time) map[string]*statefile.Unit {
	attached := attachedServices(units)
	sockets := map[string][]string{}
	for _, u := range units {
		if u.Kind == unit.KindSocket {
			sockets[u.Service] = append(sockets[u.Service], u.Name)
		}
	}
	status := make(map[string]*statefile.Unit, len(units))
	for _, u := range units {
		st := &statefile.Unit{
			Active: statefile.ActiveInactive,
			Sub:    statefile.SubDead,
			Result: statefile.ResultSuccess,
			Since:  statefile.Time{Time: started},
		}
		switch u.Kind {
		case unit.KindSocket:
			st.Type = statefile.TypeSocket
			st.Service = u.Service
		default:
			st.Type = u.Type.String()
			if socks := sockets[u.Name]; len(socks) > 0 {
				sort.Strings(socks)
				st.Sockets = socks
			}
			// In Run's order: a skipped service never starts, then
			// a socket's service waits for its socket, then an
			// OnFailure=-only oneshot for a failure.
			switch {
			case u.Condition.Skip:
			case attached[u.Name]:
				st.Activation = statefile.ActivationSocket
			case deferredOnFailure(units, u):
				st.Activation = statefile.ActivationOnFailure
			}
		}
		if u.Condition.Skip {
			st.Never = statefile.NeverCondition
		}
		status[u.Name] = st
	}
	return status
}

// update applies f to name's state under s.mu and signals the change.
func (s *Supervisor) update(name string, f func(st *statefile.Unit)) {
	s.mu.Lock()
	st := s.status[name]
	if st == nil {
		s.mu.Unlock()
		return
	}
	active, sub := st.Active, st.Sub
	f(st)
	if st.Active != active || st.Sub != sub {
		st.Since = statefile.Time{Time: time.Now()}
	}
	s.mu.Unlock()
	s.stateChanged()
}

// stateChanged tells the state writer there is something new, without
// waiting: one pending signal covers any number of changes.
func (s *Supervisor) stateChanged() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Supervisor) setState(name, active, sub string) {
	s.update(name, func(st *statefile.Unit) { st.Active, st.Sub = active, sub })
}

// admitStart counts a start of service u against its start limit and,
// if admitted, records the run: every start path comes through here, so
// runs counts each start once. restart says Restart= started it again.
func (s *Supervisor) admitStart(u *unit.Unit, restart bool) bool {
	if !s.allowStart(u) {
		return false
	}
	s.update(u.Name, func(st *statefile.Unit) {
		st.Runs++
		if restart {
			st.Restarts++
		}
		st.Active, st.Sub = statefile.ActiveActivating, statefile.SubStart
	})
	return true
}

// runResult is the result a finished run records.
func runResult(es pid1.ExitStatus) string {
	switch {
	case es.Signaled:
		return statefile.ResultSignal
	case es.ExitCode != 0:
		return statefile.ResultExitCode
	}
	return statefile.ResultSuccess
}

// recordExit records how a run of service u that was not stopped by
// shutdown ended: its result was set as it exited (finish, startFailed),
// and restarting says whether Restart= will start it again.
func (s *Supervisor) recordExit(u *unit.Unit, exitErr error, restarting bool) {
	if errors.Is(exitErr, errStopping) {
		s.recordStopped(u.Name)
		return
	}
	switch {
	case restarting:
		s.setState(u.Name, statefile.ActiveActivating, statefile.SubAutoRestart)
	case exitErr != nil:
		s.setState(u.Name, statefile.ActiveFailed, statefile.SubFailed)
	case u.Type == unit.TypeOneshot && u.RemainAfterExit:
		s.setState(u.Name, statefile.ActiveActive, statefile.SubExited)
	default:
		s.setState(u.Name, statefile.ActiveInactive, statefile.SubDead)
	}
}

// recordStopped records a unit stopped by reverse shutdown (or never
// started because it had begun). A unit already failed stays failed.
func (s *Supervisor) recordStopped(name string) {
	s.update(name, func(st *statefile.Unit) {
		if st.Active != statefile.ActiveFailed && st.Active != statefile.ActiveInactive {
			st.Active, st.Sub = statefile.ActiveInactive, statefile.SubDead
		}
	})
}

// recordStopping records that shutdown has signalled name's process.
func (s *Supervisor) recordStopping(name string) {
	s.update(name, func(st *statefile.Unit) {
		if st.Active == statefile.ActiveActive || st.Active == statefile.ActiveActivating {
			st.Active, st.Sub = statefile.ActiveDeactivating, statefile.SubStop
		}
	})
}

// recordFailed fails name for good with result.
func (s *Supervisor) recordFailed(name, result string) {
	s.update(name, func(st *statefile.Unit) {
		st.Active, st.Sub, st.Result = statefile.ActiveFailed, statefile.SubFailed, result
	})
}

// recordNever records that name will not start this boot, and why.
func (s *Supervisor) recordNever(name, never string) {
	s.update(name, func(st *statefile.Unit) { st.Never = never })
}

// State returns a copy of every unit's current state.
func (s *Supervisor) State() map[string]statefile.Unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Supervisor) stateLocked() map[string]statefile.Unit {
	out := make(map[string]statefile.Unit, len(s.status))
	for name, st := range s.status {
		out[name] = *st
	}
	return out
}
