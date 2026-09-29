// Package supervisor runs goroutine-per-service supervision against a
// loaded unit set. Each service runs in its own goroutine; sockets get
// their own goroutine that binds the listener and either waits for
// first-connect (native) or proxies bytes to the helper (proxy).
//
// The supervisor owns the start order, the restart policy, the
// OnFailure= chain, and the reverse-shutdown sequence. Process-life
// responsibilities (fork-exec, wait4 reaping, signal forwarding) live
// in pid1/Dispatcher; the supervisor's spawnAndWait is the consumer
// side of that dispatcher.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/execwrap"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/socketact"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/internal/userdb"
	"github.com/emrul/container-init/unit"
)

// Supervisor coordinates the lifecycle of every loaded unit.
type Supervisor struct {
	units      []*unit.Unit
	byName     map[string]*unit.Unit
	tracer     *trace.Tracer
	dispatcher *pid1.Dispatcher
	cgroup     *cgroup.Manager
	postLabels string // CONTAINER_INIT_TRACE_LABELS -- image policy
	euid, egid uint32 // container-init's own identity; see credentialPlan
	stopOnce   sync.Once
	stopCh     chan struct{}
	// stopTimeout bounds the whole reverse shutdown; see SetStopTimeout.
	stopTimeout time.Duration
	// spawnGate orders unit spawns against shutdown. A spawn holds it
	// shared from its noSpawn check until its process is recorded in
	// services; shutdown takes it exclusively to set noSpawn, so every
	// process that started is one its stop jobs can see, and none
	// starts after.
	spawnGate sync.RWMutex
	noSpawn   bool
	doneCh    chan struct{}

	mu       sync.Mutex
	services map[string]*serviceState
	bounds   map[string]*socketact.Bound

	// ready is one channel per loaded unit; closing it tells
	// After= / Requires= dependents that the unit reached the
	// "ready for use" state. For Type=simple/forking that's
	// post-fork-exec; for Type=oneshot that's after the first
	// successful exec; for sockets that's post-bind; for skipped
	// units that's immediately on Run() entry. See signalReady.
	ready map[string]chan struct{}
	// failed records units whose ready channel was closed by
	// markFailed rather than signalReady: they gave up before ever
	// becoming ready. waitDeps reads it once the channel is closed.
	failed map[string]bool
	// limits holds each unit's start-limit state, shared by every path
	// that starts it; see allowStart.
	limits map[string]*startLimiter
	// running holds one lock per unit, taken by whichever path is
	// running it -- its own loop, a socket activation, or an OnFailure=
	// invocation -- for as long as it does, so a unit never has two
	// processes. Fixed at New.
	running map[string]*sync.Mutex
	// missingReq records, for each unit whose Requires= chain reaches a
	// unit that is not loaded, the first such name and the unit that
	// requires it. Fixed at New; see unit.MissingRequirements.
	missingReq map[string]unit.MissingRequirement
	// status is every unit's reported state (see state.go), guarded by
	// mu; changed signals the state writer, never blocking.
	status  map[string]*statefile.Unit
	started time.Time
	changed chan struct{}
	// cgroupProbe decides, once, whether children can be spawned
	// straight into their cgroup; see canSpawnIntoCgroup.
	cgroupProbe       sync.Once
	spawnIntoCgroupOK bool
	// wrapper is the program every spawn execs first (see execwrap):
	// container-init's own binary, which then chdirs and execs the
	// service. A field so tests can substitute a copy other users can
	// execute.
	wrapper string
}

type serviceState struct {
	name      string
	pid       int
	startedAt time.Time
	restarts  int
	lastExit  pid1.ExitStatus
	lastErr   error
	exited    bool // set true when the dispatcher delivers an exit status
	// inCgroup says the process started in (or was moved into) the
	// unit's cgroup, so cgroup.kill reaches it. False when cgroup v2 is
	// off or placing it failed: it is then killed by pid and group.
	inCgroup bool
}

// New constructs a supervisor for the given units. The dispatcher
// must be Started by the caller before Run is invoked. Pass nil for
// tracer to disable tracing. Pass nil for cg to disable cgroup-v2
// integration; the supervisor falls back to the legacy SIGTERM-only
// PGID path in that case.
func New(units []*unit.Unit, tracer *trace.Tracer, dispatcher *pid1.Dispatcher, cg *cgroup.Manager) (*Supervisor, error) {
	if dispatcher == nil {
		return nil, fmt.Errorf("supervisor: dispatcher is required")
	}
	if cg == nil {
		cg = &cgroup.Manager{}
	}
	ordered, err := checkUnits(units)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*unit.Unit, len(ordered))
	for _, u := range ordered {
		byName[u.Name] = u
	}
	skipSocketsOfSkippedServices(ordered, byName)
	ready := make(map[string]chan struct{}, len(ordered))
	limits := make(map[string]*startLimiter, len(ordered))
	running := make(map[string]*sync.Mutex, len(ordered))
	started := time.Now()
	for _, u := range ordered {
		ready[u.Name] = make(chan struct{})
		limits[u.Name] = newStartLimiter(u)
		running[u.Name] = new(sync.Mutex)
	}
	return &Supervisor{
		units:       ordered,
		byName:      byName,
		tracer:      tracer,
		dispatcher:  dispatcher,
		cgroup:      cg,
		postLabels:  os.Getenv("CONTAINER_INIT_TRACE_LABELS"),
		euid:        uint32(os.Geteuid()),
		egid:        uint32(os.Getegid()),
		stopCh:      make(chan struct{}),
		stopTimeout: defaultStopTimeout,
		doneCh:      make(chan struct{}),
		services:    make(map[string]*serviceState),
		bounds:      make(map[string]*socketact.Bound),
		ready:       ready,
		failed:      make(map[string]bool),
		limits:      limits,
		running:     running,
		missingReq:  unit.MissingRequirements(byName),
		status:      initStatus(ordered, started),
		started:     started,
		changed:     make(chan struct{}, 1),
		wrapper:     "/proc/self/exe",
	}, nil
}

// signalReady closes the ready channel for unit, unblocking any
// runService goroutine that's waiting on its After= / Requires=. Safe
// to call repeatedly: the first of signalReady / markFailed to reach a
// unit settles it and later calls are no-ops.
func (s *Supervisor) signalReady(name string) {
	s.settle(name, false)
}

// markFailed settles a unit that gave up without becoming ready -- it
// failed to start, or a oneshot exited non-zero, and its Restart=
// policy will not try again. Dependents are released like signalReady
// does, but waitDeps fails those that Requires= it. Reports whether
// the unit was still unsettled; a unit that became ready earlier (a
// Type=simple that started, then died) is left ready.
func (s *Supervisor) markFailed(name string) bool {
	return s.settle(name, true)
}

func (s *Supervisor) settle(name string, failed bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.ready[name]
	if !ok {
		return false
	}
	select {
	case <-ch:
		return false // already settled
	default:
	}
	if failed {
		s.failed[name] = true
	}
	close(ch)
	return true
}

// waitDeps blocks until every unit listed in u.After has settled (or
// the supervisor is stopping). Only ordering dependencies block:
// Requires= and Wants= alone start in parallel with the unit, as in
// systemd.
//
// A dependency that failed releases units that only order After= it,
// matching systemd. A unit that both Requires= and is ordered After= it
// must not start, and waitDeps returns ok=false with failedDep naming
// it. A Requires= without After= does not stop the unit: by the time
// the requirement fails the unit may already be running, which is
// systemd's rule too. ok=false with an empty failedDep means the
// supervisor stopped first.
//
// A Requires= chain that reaches a unit that is not loaded fails at
// once, whatever the ordering: systemd cannot build the start
// transaction and does not start the unit ("Unit X not found"). Missing
// After= / Wants= names are ignored, as in systemd.
func (s *Supervisor) waitDeps(u *unit.Unit) (failedDep string, ok bool) {
	if m, ok := s.missingReq[u.Name]; ok {
		return m.Name, false
	}
	for _, name := range u.After {
		s.mu.Lock()
		ch, known := s.ready[name]
		s.mu.Unlock()
		if !known {
			continue // not loaded: nothing to order against (matches addEdge in topoSort)
		}
		select {
		case <-ch:
		case <-s.stopCh:
			return "", false
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range u.Requires {
		if s.failed[name] && slices.Contains(u.After, name) {
			return name, false
		}
	}
	return "", true
}

// dependencyFailed records that u will not start because a unit it
// Requires= failed, and passes the failure on to u's own dependents.
// As in systemd, this is not a failure of u itself: OnFailure= and
// ExitContainerOnFailure= do not fire.
func (s *Supervisor) dependencyFailed(u *unit.Unit, dep string) {
	s.logDependencyFailed(u, dep)
	s.markFailed(u.Name)
}

// logDependencyFailed logs that u was not started because its
// requirement dep failed or does not exist.
func (s *Supervisor) logDependencyFailed(u *unit.Unit, dep string) {
	reason := "failed"
	if _, known := s.byName[dep]; !known {
		reason = "not found"
		if m := s.missingReq[u.Name]; m.Name == dep && m.Via != u.Name {
			reason += " (required by " + m.Via + ")"
		}
	}
	log.Printf("unit %s: not started: required unit %s %s", u.Name, dep, reason)
	s.event(u.Name, "dependency_failed", map[string]any{"dependency": dep, "reason": reason})
	// A failed unit stays failed, and a missing one missing, for the
	// rest of the boot: u will not start.
	never := statefile.NeverDependency
	if _, known := s.byName[dep]; !known {
		never = statefile.NeverMissingRequirement
	}
	s.recordNever(u.Name, never)
}

// Run starts every non-skipped unit and blocks until Stop is invoked
// or every long-running unit has exited (whichever comes first).
// Returns the container exit code.
func (s *Supervisor) Run() int {
	defer close(s.doneCh)

	if s.euid != 0 {
		log.Printf("running as uid %d gid %d, not root: User= units must resolve to this identity; switching is off", s.euid, s.egid)
	}
	for _, env := range []string{userdb.PasswdFileEnv, userdb.GroupFileEnv} {
		if v := os.Getenv(env); v != "" {
			log.Printf("%s=%s: User= / Group= resolve against it", env, v)
		}
	}

	bindFailed := map[string]bool{}
	// Pass 1: bind every .socket whose conditions allow. Skipped
	// sockets (and their attached services) immediately signal ready
	// so dependents that After= / Requires= them don't block forever.
	// A socket whose service is skipped was skipped too, by New (Pass 2
	// logs the service's own skip).
	for _, u := range s.units {
		if u.Kind != unit.KindSocket {
			continue
		}
		if u.Condition.Skip {
			log.Printf("unit %s: skipped (%s)", u.Name, u.Condition.Reason)
			s.event(u.Name, "skipped", map[string]any{"reason": u.Condition.Reason})
			s.signalReady(u.Name)
			if u.Service != "" {
				s.signalReady(u.Service)
			}
			continue
		}
		if !s.bindSocket(u) {
			s.bindFailed(u)
			bindFailed[u.Name] = true
			continue
		}
		s.signalReady(u.Name)
		// Socket-attached service is "ready" the moment the socket
		// listens -- that's the contract of socket activation: clients
		// connect, kernel queues, helper cold-starts on demand.
		if u.Service != "" {
			s.signalReady(u.Service)
		}
	}

	// Pass 2: start every non-socket-attached, non-skipped .service.
	socketAttached := attachedServices(s.units)
	boundFor := map[string]bool{}
	for _, u := range s.units {
		if u.Kind == unit.KindSocket && !u.Condition.Skip && !bindFailed[u.Name] {
			boundFor[u.Service] = true
		}
	}
	for _, u := range s.units {
		if u.Kind != unit.KindService {
			continue
		}
		if u.Condition.Skip {
			log.Printf("unit %s: skipped (%s)", u.Name, u.Condition.Reason)
			s.event(u.Name, "skipped", map[string]any{"reason": u.Condition.Reason})
			s.signalReady(u.Name)
			continue
		}
		if socketAttached[u.Name] {
			// Pass 1 already settled socket-attached services: ready
			// once their socket bound, failed if it could not.
			if boundFor[u.Name] {
				log.Printf("unit %s: socket-activated (driven by attached .socket)", u.Name)
			}
			continue
		}
		if deferredOnFailure(s.units, u) {
			log.Printf("unit %s: deferred (OnFailure target only)", u.Name)
			s.signalReady(u.Name)
			continue
		}
		go s.runService(u)
	}

	// Boot-trace landmarks. post_services fires after the eager-spawn
	// pass dispatches every long-running goroutine (mirrors the bash
	// trace's services_invoke completion). steady_state_t+20s mirrors
	// the bash trace_mem_steady_state_async helper, capturing memory
	// after the XFCE applet wake-up settles.
	if s.tracer != nil {
		s.tracer.MemSnapshot("post_services")
		s.tracer.ScheduleMemSnapshot("steady_state_t+20s", 20*time.Second)
	}

	<-s.stopCh
	return s.shutdown()
}

// skipSocketsOfSkippedServices skips a socket whose service is
// skipped: binding it would let the first client start a service whose
// conditions said no. Done in New, so the start plan -- and the state
// the file first reports -- is fixed before anything starts.
func skipSocketsOfSkippedServices(units []*unit.Unit, byName map[string]*unit.Unit) {
	for _, u := range units {
		if u.Kind != unit.KindSocket || u.Condition.Skip {
			continue
		}
		if svc, ok := byName[u.Service]; ok && svc.Condition.Skip {
			u.Condition.Skip = true
			u.Condition.Reason = fmt.Sprintf("service %s skipped", svc.Name)
		}
	}
}

// attachedServices returns the services some non-skipped socket
// activates: Run leaves starting them to their sockets.
func attachedServices(units []*unit.Unit) map[string]bool {
	attached := map[string]bool{}
	for _, u := range units {
		if u.Kind == unit.KindSocket && !u.Condition.Skip {
			attached[u.Service] = true
		}
	}
	return attached
}

// deferredOnFailure reports whether Run leaves oneshot u to OnFailure=:
// it is named in some unit's OnFailure=, and nothing else starts it.
// Run checks skipped and socket-attached services first.
func deferredOnFailure(units []*unit.Unit, u *unit.Unit) bool {
	if u.Type != unit.TypeOneshot {
		return false
	}
	for _, other := range units {
		if slices.Contains(other.OnFailure, u.Name) {
			return true
		}
	}
	return false
}

// Stop signals the supervisor to begin reverse shutdown. Idempotent.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// stopping reports whether reverse shutdown has begun. A unit that
// exits once it has is being stopped, not failing: as in systemd,
// neither Restart= nor OnFailure= acts on it.
func (s *Supervisor) stopping() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// Done returns a channel closed when Run() has finished its shutdown.
func (s *Supervisor) Done() <-chan struct{} { return s.doneCh }

// runService owns one .service's lifecycle when it is not driven by a
// socket-activation goroutine.
func (s *Supervisor) runService(u *unit.Unit) {
	if dep, ok := s.waitDeps(u); !ok {
		if dep != "" {
			s.dependencyFailed(u, dep)
		}
		return
	}
	// Hold u's run lock for the whole loop, restarts included. It is
	// released before u's failure is handled, so an OnFailure= that
	// names u itself finds it inactive and starts it again.
	release := s.claim(u)
	defer release()
	first := true
	// Type=simple/forking: signal ready post-fork (the service is
	// "started"; long-running, never exits cleanly). The hook fires
	// inside spawnAndWait between Spawn returning and Wait blocking,
	// so a service like /bin/sleep 3600 unblocks its dependents
	// immediately rather than after sleep exits.
	var onSpawned func()
	if u.Type != unit.TypeOneshot {
		onSpawned = func() {
			if first {
				first = false
				s.signalReady(u.Name)
			}
		}
	}
	for restart := false; ; restart = true {
		select {
		case <-s.stopCh:
			s.recordStopped(u.Name)
			return
		default:
		}
		if !s.admitStart(u, restart) {
			release()
			s.startLimitHit(u)
			return
		}
		exitErr := s.spawnAndWait(u, nil, onSpawned)
		failed := exitErr != nil
		s.event(u.Name, "exited", map[string]any{"failed": failed, "err": errString(exitErr)})
		if s.stopping() {
			s.recordStopped(u.Name)
			return // stopped by reverse shutdown: not a failure
		}

		// Type=oneshot: signal ready only on first successful exec.
		// Failed oneshots stay un-ready until they succeed (or the
		// OnFailure= chain takes the container down).
		if first && u.Type == unit.TypeOneshot && !failed {
			first = false
			s.signalReady(u.Name)
		}

		if failed && u.ExitContainerOnFailure {
			s.recordExit(u, exitErr, false)
			release()
			s.unitFailed(u, exitErr)
			log.Printf("unit %s: ExitContainerOnFailure -- initiating reverse shutdown", u.Name)
			s.Stop()
			return
		}
		again := shouldRestart(u, failed)
		s.recordExit(u, exitErr, again)
		if !again {
			release()
			if failed {
				s.unitFailed(u, exitErr)
			}
			return
		}
		if u.RestartSec > 0 {
			select {
			case <-time.After(u.RestartSec):
			case <-s.stopCh:
				s.recordStopped(u.Name)
				return
			}
		}
	}
}

// claim takes u's run lock, waiting while another path runs u (an
// OnFailure= invocation, or a second socket activating the same
// service). The returned release may be called more than once.
func (s *Supervisor) claim(u *unit.Unit) (release func()) {
	run := s.running[u.Name]
	run.Lock()
	var once sync.Once
	return func() { once.Do(run.Unlock) }
}

// unitFailed handles a unit that failed and will not be started
// again -- its Restart= policy gave up, or ExitContainerOnFailure= is
// taking the container down. As in systemd, this is when OnFailure=
// fires: once the unit is failed, not on each failure it restarts
// from. A unit that never became ready (a oneshot that exited
// non-zero, or any unit that could not be started) settles as failed
// so its dependents are released or failed rather than waiting
// forever.
func (s *Supervisor) unitFailed(u *unit.Unit, err error) {
	s.markFailed(u.Name)
	// A start failure has already logged its cause.
	var se *startError
	if !errors.As(err, &se) {
		log.Printf("unit %s: failed: %v", u.Name, err)
	}
	for _, target := range u.OnFailure {
		go s.fireOnFailure(target)
	}
}

// fireOnFailure runs an OnFailure= target as a oneshot.
func (s *Supervisor) fireOnFailure(name string) {
	u, ok := s.byName[name]
	if !ok {
		log.Printf("OnFailure: unknown target %s", name)
		return
	}
	if u.Condition.Skip {
		return
	}
	if s.stopping() {
		// Queued by a failure just before shutdown began: shutdown
		// stops units, it does not run their failure handlers.
		log.Printf("OnFailure: %s not invoked: shutting down", name)
		return
	}
	// The handler's own dependencies apply as to any start: a missing
	// or failed requirement keeps it from running, and it waits for
	// the units it is ordered after.
	if dep, ok := s.waitDeps(u); !ok {
		if dep != "" {
			s.logDependencyFailed(u, dep)
		}
		return
	}
	// Starting a unit that is already active does nothing, as in
	// systemd: two failures sharing a handler that is still running
	// get the one invocation.
	run := s.running[name]
	if !run.TryLock() {
		log.Printf("OnFailure: %s not invoked: already active", name)
		s.event(name, "onfailure_coalesced", nil)
		return
	}
	defer run.Unlock()
	if !s.admitStart(u, false) {
		// A refused invocation does not chain to the target's own
		// OnFailure=, just as a failed one below does not: with a
		// cycle (A -> B -> A) and both limits spent, the two would
		// otherwise fire each other forever without starting anything.
		log.Printf("OnFailure: %s not invoked: start limit hit (%d starts within %v)",
			name, u.StartLimitBurst, u.StartLimitIntervalSec)
		s.recordFailed(name, statefile.ResultStartLimitHit)
		s.event(name, "start_limit_hit", map[string]any{
			"burst": u.StartLimitBurst, "interval_ms": u.StartLimitIntervalSec.Milliseconds(),
		})
		if u.ExitContainerOnFailure {
			log.Printf("OnFailure target %s requested container exit (start limit hit)", name)
			s.Stop()
		}
		return
	}
	log.Printf("OnFailure: invoking %s", name)
	s.event(name, "onfailure_invoke", nil)
	exitErr := s.spawnAndWait(u, nil, nil)
	if errors.Is(exitErr, errStopping) || s.stopping() {
		s.recordStopped(name)
		return
	}
	s.recordExit(u, exitErr, false)
	failed := exitErr != nil
	s.event(name, "onfailure_exited", map[string]any{"failed": failed, "err": errString(exitErr)})
	if u.ExitContainerOnFailure {
		log.Printf("OnFailure target %s requested container exit (failed=%v)", name, failed)
		s.Stop()
	}
}

// spawnAndWait runs u's ExecStart once. If extra is non-nil, the
// service is socket-activated in native mode and the listening fd is
// passed via socketact.PrepareNative. onSpawned (if non-nil) fires
// once the service's ExecStart has been exec'd -- callers use this to
// mark Type=simple/forking services ready as soon as the fork-exec
// succeeds, without waiting for the long-running process to exit.
func (s *Supervisor) spawnAndWait(u *unit.Unit, extra *socketact.Bound, onSpawned func()) error {
	if len(u.ExecStart) == 0 {
		return s.startFailed(u, fmt.Errorf("empty ExecStart"))
	}
	cmd := exec.Command(u.ExecStart[0], u.ExecStart[1:]...)
	// Build env in systemd's documented precedence:
	//   inherited PID 1 env  <  Environment=  <  EnvironmentFile= (in order)
	// Last-write-wins on the resulting slice gives the files the final
	// say, and a later file beats an earlier one, as systemd.exec(5)
	// specifies. Files marked IgnoreMissing (`-/path` syntax) are
	// silently skipped when absent; any other read error fails the unit.
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, u.Environment...)
	for _, ef := range u.EnvironmentFile {
		entries, err := unit.ParseEnvironmentFile(ef.Path)
		if err != nil {
			if os.IsNotExist(err) && ef.IgnoreMissing {
				continue
			}
			return s.startFailed(u, fmt.Errorf("env file %s: %w", ef.Path, err))
		}
		cmd.Env = append(cmd.Env, entries...)
	}
	// Per-unit log tagging. exec.Cmd's internal io.Copy goroutines
	// drain the child's stdout/stderr into these writers, which inject
	// "[unit] " in front of every newline-terminated line. The
	// goroutines exit when the child closes the pipes (i.e. on exit),
	// so we don't need to call cmd.Wait -- the pid1 dispatcher still
	// owns reaping. Mimics journald's _SYSTEMD_UNIT= grouping for
	// people grepping the container log.
	prefix := "[" + unitLabel(u.Name) + "] "
	cmd.Stdout = newLinePrefixWriter(prefix, os.Stdout)
	cmd.Stderr = newLinePrefixWriter(prefix, os.Stderr)
	cmd.SysProcAttr = procAttr()
	if u.WorkingDirectory != "" {
		cmd.Dir = u.WorkingDirectory
	}
	if u.User == "" && u.Group != "" {
		// Group= alone changes only the group, as in systemd: the uid
		// stays container-init's own, and supplementary groups are
		// dropped. Without root, the gid must already be ours.
		gid, err := userdb.GID(u.Group)
		if err != nil {
			return s.startFailed(u, fmt.Errorf("privilege drop: %w", err))
		}
		switchID, err := credentialPlan(s.euid, s.egid, userdb.Identity{UID: s.euid, GID: gid})
		if err != nil {
			return s.startFailed(u, err)
		}
		if switchID {
			applyCredential(cmd.SysProcAttr, s.euid, gid, nil)
		}
	}
	if u.User != "" {
		id, err := userdb.Resolve(u.User, u.Group, "")
		if err != nil {
			return s.startFailed(u, fmt.Errorf("privilege drop: %w", err))
		}
		switchID, err := credentialPlan(s.euid, s.egid, id)
		if err != nil {
			return s.startFailed(u, err)
		}
		if switchID {
			applyCredential(cmd.SysProcAttr, id.UID, id.GID, id.SupplementaryGroups)
		}
		// Replace HOME / USER / LOGNAME with the resolved identity's
		// values. This wins over both the inherited container-init
		// environment AND any matching key in u.Environment from the
		// unit file -- User= is the source of truth for who the
		// process is, so its environment should match. If a unit
		// genuinely needs a divergent HOME (rare), it should set
		// WorkingDirectory and the script can compute its own.
		cmd.Env = setEnv(cmd.Env, "HOME", id.Home)
		cmd.Env = setEnv(cmd.Env, "USER", id.Username)
		cmd.Env = setEnv(cmd.Env, "LOGNAME", id.Username)
	}

	if extra != nil {
		if err := socketact.PrepareNative(cmd, extra); err != nil {
			return s.startFailed(u, err)
		}
	}

	// Exec the wrapper rather than the service (see execwrap): nothing
	// between fork and exec may touch a path outside PID 1's control,
	// because the dispatcher lock is held until the exec. The wrapper
	// starts in "/" and does the chdir and the service's execve itself.
	if cmd.Err != nil {
		return s.startFailed(u, cmd.Err) // ExecStart name not found in $PATH
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return s.startFailed(u, fmt.Errorf("status pipe: %w", err))
	}
	defer statusR.Close()
	defer statusW.Close() // closed early below; this covers the error returns
	cmd.ExtraFiles = append(cmd.ExtraFiles, statusW)
	statusFD := 3 + len(cmd.ExtraFiles) - 1
	cmd.Args = execwrap.Argv(statusFD, cmd.Dir, cmd.Path, cmd.Args)
	cmd.Path = s.wrapper
	cmd.Dir = "/"

	// Create the per-unit cgroup and start the child inside it, so a
	// service that forks at once cannot leave children outside it (and
	// beyond cgroup.kill). Mkdir is idempotent across restarts.
	placed := false
	if s.cgroup.Available() {
		if dir, err := s.cgroup.Mkdir(u.Name); err != nil {
			log.Printf("cgroup mkdir %s: %v (falling back to PGID kill path)", u.Name, err)
		} else if s.canSpawnIntoCgroup() {
			f, err := os.Open(dir)
			if err != nil {
				log.Printf("cgroup open %s: %v (placing after spawn)", dir, err)
			} else {
				defer f.Close() // the child has its cgroup once Spawn returns
				spawnIntoCgroup(cmd.SysProcAttr, int(f.Fd()))
				placed = true
			}
		}
	}

	// The gate covers only the spawn and its recording, never trace
	// output: a trace destination that blocks must not hold up
	// shutdown (see shutdown).
	invokePhase := s.tracer.Begin(trace.PhaseFromUnitName(u.Name))
	s.spawnGate.RLock()
	if s.noSpawn {
		s.spawnGate.RUnlock()
		return errStopping
	}
	pid, exitCh, err := s.dispatcher.Spawn(cmd)
	// The child holds its own copy of the write end; ours must go so
	// the read below sees EOF when the service execs.
	statusW.Close()
	if err != nil {
		s.spawnGate.RUnlock()
		invokePhase.EndStatus("error", map[string]any{"unit": u.Name, "err": err.Error()})
		return s.startFailed(u, err)
	}
	// Fallback when the child could not be spawned into its cgroup:
	// migrate it now. Future fork(2)s inherit the cgroup, but anything
	// the child forked before this Place lands stays in container-init's
	// cgroup, out of reach of cgroup.kill.
	inCgroup := placed
	if s.cgroup.Available() && !placed {
		if err := s.cgroup.Place(u.Name, pid); err != nil {
			log.Printf("cgroup place %s pid=%d: %v (killing it by process group)", u.Name, pid, err)
		} else {
			inCgroup = true
		}
	}
	// Release Go's pidfd handle / process state. We own this PID via
	// the dispatcher; cmd.Wait would race the dispatcher's wait4 and
	// is never called. After Release, cmd.Process.Pid==-1, so
	// signalling MUST go through syscall.Kill on the captured pid
	// rather than cmd.Process.Kill.
	_ = cmd.Process.Release()

	state := &serviceState{name: u.Name, pid: pid, startedAt: time.Now(), inCgroup: inCgroup}
	s.mu.Lock()
	s.services[u.Name] = state
	s.mu.Unlock()
	s.spawnGate.RUnlock()

	status := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(statusR)
		status <- string(b)
	}()
	s.event(u.Name, "spawn", map[string]any{"argv": u.ExecStart, "pid": pid})
	invokePhase.End(map[string]any{"unit": u.Name, "pid": pid})
	if label := trace.PostSpawnLabel(s.postLabels, u.Name); label != "" {
		s.tracer.MemSnapshot(label)
	}

	// Wait for the wrapper to exec the service (EOF on the status pipe)
	// or to report why it could not. This can take as long as a hung
	// WorkingDirectory= mount, but only this unit waits: the dispatcher
	// lock is long gone. Stop still reaches it through the pid.
	var msg string
	select {
	case msg = <-status:
	case <-s.stopCh:
		// Still the wrapper, not the service: nothing to stop in
		// order, so end it now.
		_ = syscall.Kill(pid, syscall.SIGTERM)
		msg = <-status
	}
	if msg != "" {
		s.finish(u, state, <-exitCh)
		return s.startFailed(u, errors.New(msg))
	}
	// The service is running. A oneshot stays activating for its whole
	// run: it is done, not active, once it exits.
	if u.Type != unit.TypeOneshot {
		s.setState(u.Name, statefile.ActiveActive, statefile.SubRunning)
	}

	if onSpawned != nil {
		onSpawned()
	}

	// Reverse shutdown signals the service in its turn (see shutdown);
	// until then it keeps running, so wait for its exit either way.
	return s.finish(u, state, <-exitCh)
}

// finish records that u's process has exited with es, sweeps anything
// it left behind, and returns the exit as an error (nil for success).
func (s *Supervisor) finish(u *unit.Unit, state *serviceState, es pid1.ExitStatus) error {
	exitErr := es.AnyError()
	s.mu.Lock()
	state.exited = true
	state.lastExit = es
	state.lastErr = exitErr
	// A process stopped by reverse shutdown was not a failed run: as
	// for Restart= and OnFailure=, its exit is not a result.
	if st := s.status[u.Name]; st != nil && !s.stopping() {
		st.Result = runResult(es)
	}
	s.mu.Unlock()
	s.stateChanged()
	// Best-effort orphan cleanup. With the process in its cgroup we own
	// an atomic "kill everything in this cgroup" lever and use it; on
	// restart the cgroup will be re-populated cleanly by the next
	// spawn. Otherwise we fall back to a SIGTERM-only PGID nudge (PID
	// reuse rules out a delayed SIGKILL).
	if !state.inCgroup || s.cgroup.Kill(u.Name) != nil {
		_ = killGroup(state.pid, syscall.SIGTERM)
	}
	return exitErr
}

// bindSocket opens the listener for u and starts the right activation
// goroutine. Reports false if the listener could not be opened.
func (s *Supervisor) bindSocket(u *unit.Unit) bool {
	if len(u.ListenStream) == 0 {
		return true
	}
	l := u.ListenStream[0] // one ListenStream per .socket
	perm, err := socketPerm(u)
	var bound *socketact.Bound
	if err == nil {
		bound, err = socketact.Bind(l, perm)
	}
	if err != nil {
		log.Printf("socket %s: bind failed: %v", u.Name, err)
		s.event(u.Name, "bind_failed", map[string]any{"err": err.Error()})
		return false
	}
	s.listening(u, bound)
	return true
}

// socketPerm resolves u's SocketUser= / SocketGroup= / SocketMode=. As
// in systemd, SocketUser= alone gives the node that user's group.
func socketPerm(u *unit.Unit) (socketact.Perm, error) {
	p := socketact.Perm{Mode: u.SocketMode, ModeSet: u.SocketModeSet, UID: -1, GID: -1}
	if u.SocketUser != "" {
		id, err := userdb.Resolve(u.SocketUser, u.SocketGroup, "")
		if err != nil {
			return p, fmt.Errorf("SocketUser=: %w", err)
		}
		p.UID, p.GID = int(id.UID), int(id.GID)
	} else if u.SocketGroup != "" {
		gid, err := userdb.GID(u.SocketGroup)
		if err != nil {
			return p, fmt.Errorf("SocketGroup=: %w", err)
		}
		p.GID = int(gid)
	}
	return p, nil
}

// listening records u's bound listener and starts its activation
// goroutine.
func (s *Supervisor) listening(u *unit.Unit, bound *socketact.Bound) {
	l := u.ListenStream[0]
	s.mu.Lock()
	s.bounds[u.Name] = bound
	s.mu.Unlock()
	s.update(u.Name, func(st *statefile.Unit) {
		st.Runs++
		st.Active, st.Sub = statefile.ActiveActive, statefile.SubListening
	})
	log.Printf("socket %s: bound %s/%s -> %s (mode=%s)",
		u.Name, l.Network, l.Address, u.Service, u.ActivationMode)
	s.event(u.Name, "bound", map[string]any{
		"network": l.Network, "address": l.Address, "mode": u.ActivationMode.String(),
	})
	switch u.ActivationMode {
	case unit.ActivationNative:
		go s.driveNative(u, bound)
	case unit.ActivationProxy:
		go s.driveProxy(u, bound)
	}
}

// bindFailed fails a socket that could not bind, as systemd does: units
// that Requires= and are ordered after it do not start, and neither
// does its service, which requires its socket; the socket's OnFailure=
// fires.
func (s *Supervisor) bindFailed(sock *unit.Unit) {
	s.markFailed(sock.Name)
	s.recordFailed(sock.Name, statefile.ResultResources)
	if svc, ok := s.byName[sock.Service]; ok {
		s.logDependencyFailed(svc, sock.Name)
		s.markFailed(svc.Name)
	}
	for _, target := range sock.OnFailure {
		go s.fireOnFailure(target)
	}
}

// activationEnd says why a socket-activated service stopped running.
type activationEnd int

const (
	endStopped    activationEnd = iota // the supervisor is stopping
	endIdle                            // exited and not restarted: listen again
	endStartLimit                      // hit its start limit: the socket fails
	endDependency                      // a unit it requires failed: the socket fails
	endExit                            // ExitContainerOnFailure= is taking the container down
)

// afterActivation says what a socket does once its service's
// activation ended with end: listen again, fail with reason, or
// neither because the container is going down.
func afterActivation(svc *unit.Unit, end activationEnd) (rearm bool, fail *socketFailure) {
	switch end {
	case endIdle:
		return true, nil
	case endStartLimit:
		return false, &socketFailure{svc.Name + " hit its start limit", statefile.ResultServiceStartLimitHit}
	case endDependency:
		return false, &socketFailure{svc.Name + " cannot start: a unit it requires failed", statefile.ResultDependency}
	}
	return false, nil
}

// socketFailure is why a socket fails: the logged reason, and the
// result its state records.
type socketFailure struct {
	reason, result string
}

// driveNative waits for the listener to become readable, then execs
// the service with the listening fd inherited as fd 3, restarting it
// per its Restart= policy. Once the service stops and is not restarted,
// the socket listens again and the next connection starts it anew, as
// in systemd. It fails, closing the listener, when the service hits
// its start limit, when a unit the service requires failed, or when
// the socket's own trigger limit is spent.
func (s *Supervisor) driveNative(sock *unit.Unit, bound *socketact.Bound) {
	svc, ok := s.byName[sock.Service]
	if !ok {
		log.Printf("socket %s: unknown service %s", sock.Name, sock.Service)
		return
	}
	for first := true; ; first = false {
		if err := bound.WaitReadable(s.stopCh); err != nil {
			return
		}
		end := endIdle
		if first {
			// Honour the helper service's After= before its first
			// spawn -- the socket has been listening since Pass 1, so a
			// client may have queued bytes already; we still don't exec
			// the helper until prerequisite oneshot units have completed.
			if dep, ok := s.waitDeps(svc); !ok {
				if dep == "" {
					return
				}
				s.logDependencyFailed(svc, dep)
				end = endDependency
			}
		}
		if end == endIdle {
			if fail := s.trigger(sock, first); fail != nil {
				s.socketFailed(sock, bound, fail)
				return
			}
			end = s.runActivated(svc, bound)
		}
		rearm, fail := afterActivation(svc, end)
		if fail != nil {
			s.socketFailed(sock, bound, fail)
		}
		if !rearm {
			return
		}
		s.listeningAgain(sock, svc)
	}
}

// driveProxy keeps the public listener in container-init. A connection
// that finds no helper running starts one on its private endpoint;
// later connections reuse it. The helper restarts per its Restart=
// policy, and once it stops for good the next connection starts it
// anew. The socket fails, as in driveNative, on the helper's start
// limit, a failed requirement, or its own trigger limit.
func (s *Supervisor) driveProxy(sock *unit.Unit, bound *socketact.Bound) {
	svc, ok := s.byName[sock.Service]
	if !ok {
		log.Printf("socket %s: unknown service %s", sock.Name, sock.Service)
		return
	}
	var (
		mu    sync.Mutex
		up    chan struct{} // closed once the running helper may be dialled; nil while none runs
		first = true
		// failed is set, under mu, the moment the socket is decided
		// failed -- before its listener closes -- so no connection can
		// start a helper after that.
		failed atomic.Bool
	)
	// helper returns the running helper's channel, starting a helper if
	// none runs; false means the socket has failed or shutdown began.
	helper := func() (chan struct{}, bool) {
		mu.Lock()
		if failed.Load() {
			mu.Unlock()
			return nil, false
		}
		if up != nil {
			defer mu.Unlock()
			return up, true // a running helper serves, shutdown or not
		}
		if s.stopping() {
			mu.Unlock()
			return nil, false // but none starts once shutdown began
		}
		if fail := s.trigger(sock, first); fail != nil {
			failed.Store(true)
			mu.Unlock()
			s.socketFailed(sock, bound, fail)
			return nil, false
		}
		ch := make(chan struct{})
		up = ch
		checkDeps := first
		first = false
		mu.Unlock()
		go func() {
			end := s.runHelperLoop(svc, ch, checkDeps)
			rearm, fail := afterActivation(svc, end)
			// Idle or failed in one step: a connection sees either the
			// helper gone and the socket usable, or the socket failed.
			mu.Lock()
			up = nil
			if fail != nil {
				failed.Store(true)
			}
			mu.Unlock()
			if fail != nil {
				s.socketFailed(sock, bound, fail)
			}
			if rearm {
				s.listeningAgain(sock, svc)
			}
		}()
		return ch, true
	}

	// The listener stays open through shutdown until the socket's own
	// stop job closes it, after its service and every unit ordered
	// after it have stopped; until then connections still reach a
	// running helper.
	for {
		conn, err := bound.Accept()
		if err != nil {
			if !failed.Load() && !s.stopping() {
				log.Printf("socket %s accept: %v", sock.Name, err)
			}
			return
		}
		ch, ok := helper()
		if !ok {
			conn.Close()
			if failed.Load() {
				return
			}
			continue // shutting down: no new helper
		}
		<-ch
		network, target := splitProxyTarget(sock.ProxyTarget)
		go func(c net.Conn) {
			defer c.Close()
			err := socketact.ProxyTo(c, network, target,
				250*time.Millisecond, 5*time.Second)
			socketact.Log(sock.Name, c.RemoteAddr().String(), target, err)
		}(conn)
	}
}

// runHelperLoop runs one activation of the proxy-mode helper. helperUp
// is closed before the first spawn so the proxy can begin dialling the
// private endpoint (with retry). The helper's After= is honoured on the
// socket's first activation only; later ones find it settled.
func (s *Supervisor) runHelperLoop(svc *unit.Unit, helperUp chan struct{}, checkDeps bool) activationEnd {
	if checkDeps {
		if dep, ok := s.waitDeps(svc); !ok {
			close(helperUp) // unblock the accept loop
			if dep == "" {
				return endStopped
			}
			s.logDependencyFailed(svc, dep)
			return endDependency
		}
	}
	close(helperUp)
	return s.runActivated(svc, nil)
}

// runActivated runs a socket-activated service, restarting it per its
// Restart= policy, until it stops for good. extra is the listener to
// pass in native mode, nil in proxy mode.
func (s *Supervisor) runActivated(svc *unit.Unit, extra *socketact.Bound) activationEnd {
	// As in runService, svc's run lock is held for the activation and
	// released before its failure is handled.
	release := s.claim(svc)
	defer release()
	for restart := false; ; restart = true {
		select {
		case <-s.stopCh:
			s.recordStopped(svc.Name)
			return endStopped
		default:
		}
		if !s.admitStart(svc, restart) {
			release()
			s.startLimitHit(svc)
			return endStartLimit
		}
		exitErr := s.spawnAndWait(svc, extra, nil)
		failed := exitErr != nil
		s.event(svc.Name, "exited", map[string]any{"failed": failed, "err": errString(exitErr)})
		if s.stopping() {
			s.recordStopped(svc.Name)
			return endStopped
		}
		if failed && svc.ExitContainerOnFailure {
			s.recordExit(svc, exitErr, false)
			release()
			s.unitFailed(svc, exitErr)
			s.Stop()
			return endExit
		}
		again := shouldRestart(svc, failed)
		s.recordExit(svc, exitErr, again)
		if !again {
			release()
			if failed {
				s.unitFailed(svc, exitErr)
			}
			return endIdle
		}
		if svc.RestartSec > 0 {
			select {
			case <-time.After(svc.RestartSec):
			case <-s.stopCh:
				s.recordStopped(svc.Name)
				return endStopped
			}
		}
	}
}

// trigger counts one activation of sock against its trigger limit. It
// returns why the socket must fail when the limit is spent, or nil.
func (s *Supervisor) trigger(sock *unit.Unit, first bool) *socketFailure {
	if !s.allowStart(sock) {
		return &socketFailure{fmt.Sprintf("trigger limit hit (%d activations within %v)",
			sock.TriggerLimitBurst, sock.TriggerLimitIntervalSec), statefile.ResultTriggerLimitHit}
	}
	if first {
		s.event(sock.Name, "first_connect", nil)
	} else {
		s.event(sock.Name, "triggered", nil)
	}
	return nil
}

// listeningAgain logs that sock is back to waiting for a connection
// after svc stopped.
func (s *Supervisor) listeningAgain(sock, svc *unit.Unit) {
	log.Printf("socket %s: %s stopped; listening again", sock.Name, svc.Name)
	s.event(sock.Name, "rearmed", map[string]any{"service": svc.Name})
}

// socketFailed closes a socket that will not start its service again.
// As in systemd, the socket's own OnFailure= fires.
func (s *Supervisor) socketFailed(sock *unit.Unit, bound *socketact.Bound, fail *socketFailure) {
	log.Printf("socket %s: failed: %s; closing the listener", sock.Name, fail.reason)
	s.event(sock.Name, "socket_failed", map[string]any{"reason": fail.reason})
	s.recordFailed(sock.Name, fail.result)
	s.mu.Lock()
	delete(s.bounds, sock.Name)
	s.mu.Unlock()
	bound.Close()
	for _, target := range sock.OnFailure {
		go s.fireOnFailure(target)
	}
}

// startLimiter enforces StartLimitBurst= / StartLimitIntervalSec= the
// way systemd's ratelimit does: a window opens at the first start and
// admits burst starts; a start after the window has elapsed opens a
// new one. Every start counts, restarts and the first alike. A socket's
// limiter enforces its TriggerLimitBurst= / TriggerLimitIntervalSec=
// instead, counting activations.
type startLimiter struct {
	burst    int
	interval time.Duration
	begin    time.Time
	n        int
}

func newStartLimiter(u *unit.Unit) *startLimiter {
	if u.Kind == unit.KindSocket {
		return &startLimiter{burst: u.TriggerLimitBurst, interval: u.TriggerLimitIntervalSec}
	}
	return &startLimiter{burst: u.StartLimitBurst, interval: u.StartLimitIntervalSec}
}

// allow reports whether a start at now is within the limit, and if so
// counts it.
func (l *startLimiter) allow(now time.Time) bool {
	if l == nil || l.burst <= 0 || l.interval <= 0 {
		return true // no limit configured
	}
	if l.begin.IsZero() || now.Sub(l.begin) >= l.interval {
		l.begin, l.n = now, 0
	}
	if l.n >= l.burst {
		return false
	}
	l.n++
	return true
}

// allowStart counts a start of u against its start limit. Every path
// that starts a unit goes through here -- its own loop, each socket
// that activates it, and OnFailure= invocations -- so they share one
// budget.
func (s *Supervisor) allowStart(u *unit.Unit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits[u.Name].allow(time.Now())
}

// startLimitHit fails a unit that would exceed its start limit: it is
// not started again, and fails like any unit that gave up -- OnFailure=
// and ExitContainerOnFailure= fire, and units that Requires= it are
// failed if it never became ready.
func (s *Supervisor) startLimitHit(u *unit.Unit) {
	log.Printf("unit %s: start limit hit (%d starts within %v); not starting again",
		u.Name, u.StartLimitBurst, u.StartLimitIntervalSec)
	s.event(u.Name, "start_limit_hit", map[string]any{
		"burst": u.StartLimitBurst, "interval_ms": u.StartLimitIntervalSec.Milliseconds(),
	})
	s.markFailed(u.Name)
	s.recordFailed(u.Name, statefile.ResultStartLimitHit)
	for _, target := range u.OnFailure {
		go s.fireOnFailure(target)
	}
	if u.ExitContainerOnFailure {
		log.Printf("unit %s: ExitContainerOnFailure -- initiating reverse shutdown", u.Name)
		s.Stop()
	}
}

func shouldRestart(u *unit.Unit, failed bool) bool {
	switch u.Restart {
	case unit.RestartAlways:
		return true
	case unit.RestartOnFailure:
		return failed
	}
	return false
}

// Default stop timeouts. A unit gets defaultTimeoutStopSec unless it
// sets TimeoutStopSec=; the whole shutdown gets defaultStopTimeout
// unless SetStopTimeout says otherwise. The overall default stays
// under docker stop's 10s, after which the container is SIGKILLed with
// whatever is still running.
const (
	defaultTimeoutStopSec = 5 * time.Second
	defaultStopTimeout    = 8 * time.Second
)

// SetStopTimeout bounds the whole reverse shutdown: once d has passed,
// every unit still running is killed at once. d <= 0 restores the
// default. Call before Run.
func (s *Supervisor) SetStopTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultStopTimeout
	}
	s.stopTimeout = d
}

// shutdown stops every unit in reverse dependency order, as systemd
// does: a unit is stopped once every unit ordered After= it has
// stopped, and units with no ordering between them stop in parallel.
// Each gets its KillSignal= (default SIGTERM) and TimeoutStopSec=
// (default 5s) before it is killed, and the whole shutdown is bounded
// by the stop timeout. Returns the container exit code: 0 on graceful
// shutdown, 1 if any unit had to be killed.
func (s *Supervisor) shutdown() int {
	deadline := time.Now().Add(s.stopTimeout)
	// The trace's final flush shares this deadline instead of adding
	// to it; tracing itself never blocks shutdown.
	s.tracer.SetDeadline(deadline)
	// Nothing starts from here on. Spawns already past the gate finish
	// recording their process first, so the stop jobs below see them.
	s.spawnGate.Lock()
	s.noSpawn = true
	s.spawnGate.Unlock()
	// dependents[x] lists the units ordered after x (orderedAfter:
	// After=, Before= folded in, and each socket before its service).
	// topoSort rejected cycles in the same ordering, so every wait
	// below ends.
	dependents := make(map[string][]string, len(s.units))
	for name, deps := range orderedAfter(s.units) {
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], name)
		}
	}
	stopped := make(map[string]chan struct{}, len(s.units))
	for _, u := range s.units {
		stopped[u.Name] = make(chan struct{})
	}
	// expired closes at the deadline: from then on nothing waits for
	// order, and every unit still running is stopped (and killed) now.
	expired := make(chan struct{})
	expiry := time.AfterFunc(time.Until(deadline), func() { close(expired) })
	defer expiry.Stop()
	var forced atomic.Bool
	var wg sync.WaitGroup
	for _, u := range s.units {
		wg.Add(1)
		go func(u *unit.Unit) {
			defer wg.Done()
			defer close(stopped[u.Name])
			for _, d := range dependents[u.Name] {
				select {
				case <-stopped[d]:
				case <-expired:
				}
			}
			if u.Kind == unit.KindSocket {
				s.closeSocket(u.Name)
				return
			}
			if s.stopUnit(u, deadline) {
				forced.Store(true)
			}
		}(u)
	}
	wg.Wait()

	exit := 0
	if forced.Load() {
		exit = 1
	}
	if s.tracer != nil {
		s.tracer.Event("reverse_shutdown_done", map[string]any{"exit": exit})
	}
	return exit
}

// closeSocket closes the listener of the socket named name, if it is
// still bound: its stop job in reverse shutdown.
func (s *Supervisor) closeSocket(name string) {
	s.mu.Lock()
	b := s.bounds[name]
	delete(s.bounds, name)
	s.mu.Unlock()
	if b != nil {
		b.Close()
		s.recordStopped(name)
	}
}

// stopUnit stops u's running process, if it has one: its KillSignal=,
// then up to its TimeoutStopSec= (cut short by deadline) for it to
// exit, then a kill of its whole cgroup (or process group). Reports
// whether it had to be killed.
func (s *Supervisor) stopUnit(u *unit.Unit, deadline time.Time) (forced bool) {
	s.mu.Lock()
	st := s.services[u.Name]
	s.mu.Unlock()
	if st == nil {
		return false // never spawned
	}
	exited := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return st.exited
	}
	defer s.recordStopped(u.Name)
	if !exited() && st.pid > 0 {
		s.recordStopping(u.Name)
		sig := u.KillSignal
		if sig == 0 {
			sig = syscall.SIGTERM
		}
		_ = syscall.Kill(st.pid, sig)
		wait := u.TimeoutStopSec
		if wait <= 0 {
			wait = defaultTimeoutStopSec
		}
		end := time.Now().Add(wait)
		if deadline.Before(end) {
			end = deadline
		}
		for !exited() && time.Now().Before(end) {
			time.Sleep(20 * time.Millisecond)
		}
		if !exited() {
			// Force-kill: cgroup.kill is atomic and reaches every
			// descendant regardless of PID reuse. When the process is
			// not in its cgroup, or the kill fails, fall back to PID +
			// PGID SIGKILL (racy on PID reuse, but the best there is).
			if !st.inCgroup || s.cgroup.Kill(u.Name) != nil {
				_ = syscall.Kill(st.pid, syscall.SIGKILL)
				_ = killGroup(st.pid, syscall.SIGKILL)
			}
			log.Printf("shutdown: %s force-killed (stop timeout expired)", u.Name)
			forced = true
		}
	}
	if s.cgroup.Available() {
		// Even on graceful exit, sweep the cgroup so leftover orphans
		// (dbus-daemon etc.) don't outlive the container's reverse
		// shutdown.
		_ = s.cgroup.Kill(u.Name)
		_ = s.cgroup.Remove(u.Name)
	}
	return forced
}

// credentialPlan decides how a User= unit is started. As root,
// container-init switches to the unit's identity. Without root it cannot
// switch at all -- setgroups needs CAP_SETGID even when nothing changes,
// and a non-root container has no effective capabilities -- so a unit
// whose uid and gid are already ours runs as-is, keeping container-init's
// supplementary groups, and any other unit is refused rather than
// started as the wrong user.
func credentialPlan(euid, egid uint32, id userdb.Identity) (switchID bool, err error) {
	if euid == 0 {
		return true, nil
	}
	if id.UID == euid && id.GID == egid {
		return false, nil
	}
	return false, fmt.Errorf("unit wants uid %d gid %d, container-init runs as uid %d gid %d and cannot switch",
		id.UID, id.GID, euid, egid)
}

// canSpawnIntoCgroup reports whether spawns can use
// clone3(CLONE_INTO_CGROUP), deciding it once with a probe spawn. Kernels
// before 5.7 lack it, and seccomp profiles can refuse clone3 with
// whatever errno they choose (Docker's default answers ENOSYS, others
// EPERM). Telling that apart from a unit's own spawn failure by errno is
// not possible -- a credential switch that fails in the child is EPERM
// too -- so the probe spawns container-init's own binary into a scratch
// cgroup with no credentials, and any failure means the fallback: move
// each child into its cgroup after spawn, which leaves a window where an
// early fork escapes.
func (s *Supervisor) canSpawnIntoCgroup() bool {
	s.cgroupProbe.Do(func() {
		if err := s.probeSpawnIntoCgroup(); err != nil {
			log.Printf("cgroup: cannot spawn into a cgroup (%v); moving units into their cgroup after spawn", err)
			return
		}
		s.spawnIntoCgroupOK = true
	})
	return s.spawnIntoCgroupOK
}

func (s *Supervisor) probeSpawnIntoCgroup() error {
	const name = "spawn-probe" // no unit suffix, so no unit can share it
	dir, err := s.cgroup.Mkdir(name)
	if err != nil {
		return err
	}
	defer s.cgroup.Remove(name)
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(s.wrapper, execwrap.ProbeArg)
	cmd.Dir = "/"
	cmd.SysProcAttr = procAttr()
	spawnIntoCgroup(cmd.SysProcAttr, int(f.Fd()))
	_, exitCh, err := s.dispatcher.Spawn(cmd)
	if err != nil {
		return err
	}
	_ = cmd.Process.Release()
	if err := (<-exitCh).AnyError(); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	return nil
}

// startFailed logs a unit whose ExecStart could not be started (bad
// environment file, unresolvable User=, fork/exec error) and returns
// the error for the caller's restart / failure handling. These never
// produce child output, so without this line the container log would
// show nothing at all.
func (s *Supervisor) startFailed(u *unit.Unit, err error) error {
	log.Printf("unit %s: failed to start: %v", u.Name, err)
	s.event(u.Name, "start_failed", map[string]any{"err": err.Error()})
	s.update(u.Name, func(st *statefile.Unit) { st.Result = statefile.ResultResources })
	return &startError{unit: u.Name, err: err}
}

// errStopping is what spawnAndWait returns once shutdown has begun: the
// unit was not started. Callers check stopping() and give up quietly.
var errStopping = errors.New("not started: shutting down")

// startError is a failure to start a unit, as opposed to a unit that
// ran and exited non-zero.
type startError struct {
	unit string
	err  error
}

func (e *startError) Error() string { return "start " + e.unit + ": " + e.err.Error() }
func (e *startError) Unwrap() error { return e.err }

func (s *Supervisor) event(unitName, phase string, fields map[string]any) {
	if s.tracer == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["unit"] = unitName
	s.tracer.Event(phase, fields)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// setEnv sets key to value in env, removing every other entry for key:
// the child's environment is deduplicated with the last entry winning,
// so replacing only the first would lose to a later duplicate. Used to
// overwrite HOME / USER / LOGNAME on privilege drop so the dropped-priv
// child sees the resolved identity, not whatever container-init
// inherited as PID 1 or the unit set.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
	return append(env, prefix+value)
}

// splitProxyTarget recognises tcp:HOST:PORT, unix:/path, /path, or a
// bare HOST:PORT (defaults to tcp).
func splitProxyTarget(t string) (string, string) {
	switch {
	case strings.HasPrefix(t, "tcp:"):
		return "tcp", strings.TrimPrefix(t, "tcp:")
	case strings.HasPrefix(t, "unix:"):
		return "unix", strings.TrimPrefix(t, "unix:")
	case strings.HasPrefix(t, "/"):
		return "unix", t
	}
	return "tcp", t
}
