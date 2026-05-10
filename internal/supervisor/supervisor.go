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
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/socketact"
	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/unit"
	"github.com/emrul/container-init/internal/userdb"
)

// Supervisor coordinates the lifecycle of every loaded unit.
type Supervisor struct {
	units      []*unit.Unit
	byName     map[string]*unit.Unit
	tracer     *trace.Tracer
	dispatcher *pid1.Dispatcher
	cgroup     *cgroup.Manager
	postLabels string // CONTAINER_INIT_TRACE_LABELS -- image policy
	stopOnce   sync.Once
	stopCh     chan struct{}
	doneCh     chan struct{}

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
}

type serviceState struct {
	name      string
	pid       int
	startedAt time.Time
	restarts  int
	lastExit  pid1.ExitStatus
	lastErr   error
	exited    bool // set true when the dispatcher delivers an exit status
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
	ordered, err := topoSort(units)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*unit.Unit, len(ordered))
	for _, u := range ordered {
		byName[u.Name] = u
	}
	ready := make(map[string]chan struct{}, len(ordered))
	for _, u := range ordered {
		ready[u.Name] = make(chan struct{})
	}
	return &Supervisor{
		units:      ordered,
		byName:     byName,
		tracer:     tracer,
		dispatcher: dispatcher,
		cgroup:     cg,
		postLabels: os.Getenv("CONTAINER_INIT_TRACE_LABELS"),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		services:   make(map[string]*serviceState),
		bounds:     make(map[string]*socketact.Bound),
		ready:      ready,
	}, nil
}

// signalReady closes the ready channel for unit, unblocking any
// runService goroutine that's waiting on its After= / Requires=. Safe
// to call repeatedly -- the close is single-shot via sync.Once
// underneath.
func (s *Supervisor) signalReady(name string) {
	s.mu.Lock()
	ch, ok := s.ready[name]
	s.mu.Unlock()
	if !ok {
		return
	}
	defer func() { _ = recover() }() // close-of-closed-channel is harmless here
	select {
	case <-ch:
		// already closed
	default:
		close(ch)
	}
}

// waitDeps blocks until every unit listed in u.After / u.Requires has
// signalled ready (or the supervisor is stopping). Wants= is a soft
// hint per systemd convention; we don't block on it. Returns false
// when the supervisor stopped before deps resolved.
func (s *Supervisor) waitDeps(u *unit.Unit) bool {
	deps := append([]string(nil), u.After...)
	deps = append(deps, u.Requires...)
	for _, name := range deps {
		s.mu.Lock()
		ch, ok := s.ready[name]
		s.mu.Unlock()
		if !ok {
			continue // unknown dep -- skip silently (matches addEdge's behaviour in topoSort)
		}
		select {
		case <-ch:
		case <-s.stopCh:
			return false
		}
	}
	return true
}

// Run starts every non-skipped unit and blocks until Stop is invoked
// or every long-running unit has exited (whichever comes first).
// Returns the container exit code.
func (s *Supervisor) Run() int {
	defer close(s.doneCh)

	// Pass 1: bind every .socket whose conditions allow. Skipped
	// sockets (and their attached services) immediately signal ready
	// so dependents that After= / Requires= them don't block forever.
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
		s.bindSocket(u)
		s.signalReady(u.Name)
		// Socket-attached service is "ready" the moment the socket
		// listens -- that's the contract of socket activation: clients
		// connect, kernel queues, helper cold-starts on demand.
		if u.Service != "" {
			s.signalReady(u.Service)
		}
	}

	// Pass 2: start every non-socket-attached, non-skipped .service.
	socketAttached := map[string]bool{}
	for _, u := range s.units {
		if u.Kind == unit.KindSocket && !u.Condition.Skip {
			socketAttached[u.Service] = true
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
			log.Printf("unit %s: socket-activated (driven by attached .socket)", u.Name)
			// Pass 1 already signalled ready for socket-attached services.
			continue
		}
		if u.Type == unit.TypeOneshot && s.invokedOnlyByOnFailure(u.Name) {
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

func (s *Supervisor) invokedOnlyByOnFailure(name string) bool {
	for _, u := range s.units {
		for _, t := range u.OnFailure {
			if t == name {
				return true
			}
		}
	}
	return false
}

// Stop signals the supervisor to begin reverse shutdown. Idempotent.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// Done returns a channel closed when Run() has finished its shutdown.
func (s *Supervisor) Done() <-chan struct{} { return s.doneCh }

// runService owns one .service's lifecycle when it is not driven by a
// socket-activation goroutine.
func (s *Supervisor) runService(u *unit.Unit) {
	if !s.waitDeps(u) {
		return
	}
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
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		exitErr := s.spawnAndWait(u, nil, onSpawned)
		failed := exitErr != nil
		s.event(u.Name, "exited", map[string]any{"failed": failed, "err": errString(exitErr)})

		// Type=oneshot: signal ready only on first successful exec.
		// Failed oneshots stay un-ready until they succeed (or the
		// OnFailure= chain takes the container down).
		if first && u.Type == unit.TypeOneshot && !failed {
			first = false
			s.signalReady(u.Name)
		}

		if failed {
			for _, target := range u.OnFailure {
				go s.fireOnFailure(target)
			}
		}
		if failed && u.ExitContainerOnFailure {
			log.Printf("unit %s: ExitContainerOnFailure -- initiating reverse shutdown", u.Name)
			s.Stop()
			return
		}
		if !shouldRestart(u, failed) {
			// Unblock any dependents that were waiting on a oneshot
			// that's now done (success or terminal failure) -- without
			// this, a Type=oneshot Restart=no that ran and exited 0
			// would not signal because the success-signal above
			// flipped first=false; that's fine. Failed oneshots stay
			// unsignalled deliberately.
			return
		}
		if u.RestartSec > 0 {
			select {
			case <-time.After(u.RestartSec):
			case <-s.stopCh:
				return
			}
		}
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
	log.Printf("OnFailure: invoking %s", name)
	s.event(name, "onfailure_invoke", nil)
	exitErr := s.spawnAndWait(u, nil, nil)
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
// after dispatcher.Spawn returns and the cgroup placement has run --
// callers use this to mark Type=simple/forking services ready as
// soon as the fork-exec succeeds, without waiting for the long-running
// process to exit.
func (s *Supervisor) spawnAndWait(u *unit.Unit, extra *socketact.Bound, onSpawned func()) error {
	if len(u.ExecStart) == 0 {
		return fmt.Errorf("unit %s: empty ExecStart", u.Name)
	}
	cmd := exec.Command(u.ExecStart[0], u.ExecStart[1:]...)
	cmd.Env = append(os.Environ(), u.Environment...)
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
	if u.User != "" {
		id, err := userdb.Resolve(u.User, u.Group, "")
		if err != nil {
			return fmt.Errorf("unit %s: privilege drop: %w", u.Name, err)
		}
		applyCredential(cmd.SysProcAttr, id.UID, id.GID, id.SupplementaryGroups)
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
			return err
		}
	}

	// Pre-create the per-unit cgroup so we can place the child into
	// it the moment fork returns. Mkdir is idempotent across restarts.
	if s.cgroup.Available() {
		if _, err := s.cgroup.Mkdir(u.Name); err != nil {
			log.Printf("cgroup mkdir %s: %v (falling back to PGID kill path)", u.Name, err)
		}
	}

	s.event(u.Name, "spawn", map[string]any{"argv": u.ExecStart})
	invokePhase := s.tracer.Begin(trace.PhaseFromUnitName(u.Name))
	pid, exitCh, err := s.dispatcher.Spawn(cmd)
	if err != nil {
		invokePhase.EndStatus("error", map[string]any{"unit": u.Name, "err": err.Error()})
		return fmt.Errorf("start %s: %w", u.Name, err)
	}
	invokePhase.End(map[string]any{"unit": u.Name, "pid": pid})
	if label := trace.PostSpawnLabel(s.postLabels, u.Name); label != "" {
		s.tracer.MemSnapshot(label)
	}
	// Migrate the child into its cgroup. Future fork(2)s by the child
	// inherit the cgroup, so any double-fork descendants land in the
	// same atomically-killable group. Race-window note: the child can
	// in principle fork before this Place lands, leaving its earliest
	// grandchild in the parent cgroup. In practice the children we
	// supervise either don't fork at all (Type=simple) or do so well
	// after the kernel has had time to schedule us (oneshots that
	// invoke dbus-launch et al), so the window is empirically empty.
	if s.cgroup.Available() {
		if err := s.cgroup.Place(u.Name, pid); err != nil {
			log.Printf("cgroup place %s pid=%d: %v", u.Name, pid, err)
		}
	}
	// Release Go's pidfd handle / process state. We own this PID via
	// the dispatcher; cmd.Wait would race the dispatcher's wait4 and
	// is never called. After Release, cmd.Process.Pid==-1, so
	// signalling MUST go through syscall.Kill on the captured pid
	// rather than cmd.Process.Kill.
	_ = cmd.Process.Release()

	state := &serviceState{name: u.Name, pid: pid, startedAt: time.Now()}
	s.mu.Lock()
	s.services[u.Name] = state
	s.mu.Unlock()

	if onSpawned != nil {
		onSpawned()
	}

	var es pid1.ExitStatus
	select {
	case <-s.stopCh:
		// Reverse shutdown -- kick the child via SIGTERM and still
		// block on the dispatcher so state.exited is honest.
		_ = syscall.Kill(pid, syscall.SIGTERM)
		es = <-exitCh
	case es = <-exitCh:
	}

	exitErr := es.AnyError()
	s.mu.Lock()
	state.exited = true
	state.lastExit = es
	state.lastErr = exitErr
	s.mu.Unlock()
	// Best-effort orphan cleanup. With cgroup-v2 we own an atomic
	// "kill everything in this cgroup" lever and use it; on restart
	// the cgroup will be re-populated cleanly by the next Place.
	// Without cgroup-v2 we fall back to a SIGTERM-only PGID nudge
	// (PID reuse rules out a delayed SIGKILL).
	if s.cgroup.Available() {
		_ = s.cgroup.Kill(u.Name)
	} else {
		_ = killGroup(pid, syscall.SIGTERM)
	}
	return exitErr
}

// bindSocket opens the listener for u and starts the right activation
// goroutine.
func (s *Supervisor) bindSocket(u *unit.Unit) {
	if len(u.ListenStream) == 0 {
		return
	}
	l := u.ListenStream[0] // one ListenStream per .socket
	bound, err := socketact.Bind(l)
	if err != nil {
		log.Printf("socket %s: bind failed: %v", u.Name, err)
		s.event(u.Name, "bind_failed", map[string]any{"err": err.Error()})
		return
	}
	s.mu.Lock()
	s.bounds[u.Name] = bound
	s.mu.Unlock()
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

// driveNative blocks until the listener is readable, then exec's the
// service with the listening fd inherited as fd 3. Restarts the
// service per its Restart= policy, re-passing the same fd each time.
func (s *Supervisor) driveNative(sock *unit.Unit, bound *socketact.Bound) {
	svc, ok := s.byName[sock.Service]
	if !ok {
		log.Printf("socket %s: unknown service %s", sock.Name, sock.Service)
		return
	}
	if err := waitReadable(int(bound.File.Fd()), s.stopCh); err != nil {
		return
	}
	// Honour the helper service's After= / Requires= before its first
	// spawn -- the socket has been listening since Pass 1, so a client
	// may have queued bytes already; we still don't exec the helper
	// until prerequisite oneshot units have completed.
	if !s.waitDeps(svc) {
		return
	}
	s.event(sock.Name, "first_connect", nil)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		exitErr := s.spawnAndWait(svc, bound, nil)
		failed := exitErr != nil
		s.event(svc.Name, "exited", map[string]any{"failed": failed, "err": errString(exitErr)})
		if failed && svc.ExitContainerOnFailure {
			s.Stop()
			return
		}
		if !shouldRestart(svc, failed) {
			return
		}
		if svc.RestartSec > 0 {
			select {
			case <-time.After(svc.RestartSec):
			case <-s.stopCh:
				return
			}
		}
	}
}

// driveProxy keeps the public listener in container-init. On first
// accept it lazy-starts the helper on its private endpoint and
// proxies bytes; subsequent connects reuse the running helper. The
// helper is restarted independently per its Restart= policy.
func (s *Supervisor) driveProxy(sock *unit.Unit, bound *socketact.Bound) {
	svc, ok := s.byName[sock.Service]
	if !ok {
		log.Printf("socket %s: unknown service %s", sock.Name, sock.Service)
		return
	}
	helperUp := make(chan struct{})
	var startOnce sync.Once
	startHelper := func() {
		startOnce.Do(func() {
			s.event(sock.Name, "first_connect", nil)
			go s.runHelperLoop(svc, helperUp)
		})
	}

	go func() {
		<-s.stopCh
		bound.Close()
	}()
	for {
		conn, err := bound.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
			}
			log.Printf("socket %s accept: %v", sock.Name, err)
			return
		}
		startHelper()
		<-helperUp
		network, target := splitProxyTarget(sock.ProxyTarget)
		go func(c net.Conn) {
			defer c.Close()
			err := socketact.ProxyTo(c, network, target,
				250*time.Millisecond, 5*time.Second)
			socketact.Log(sock.Name, c.RemoteAddr().String(), target, err)
		}(conn)
	}
}

// runHelperLoop spawns the proxy-mode helper and respawns on failure
// per its Restart= policy. helperUp is closed before the first
// spawnAndWait returns so the proxy goroutine can begin dialing the
// private endpoint (with retry).
func (s *Supervisor) runHelperLoop(svc *unit.Unit, helperUp chan struct{}) {
	if !s.waitDeps(svc) {
		close(helperUp) // unblock the accept loop; it will see stopCh next
		return
	}
	first := true
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		if first {
			close(helperUp)
			first = false
		}
		exitErr := s.spawnAndWait(svc, nil, nil)
		failed := exitErr != nil
		s.event(svc.Name, "exited", map[string]any{"failed": failed, "err": errString(exitErr)})
		if failed && svc.ExitContainerOnFailure {
			s.Stop()
			return
		}
		if !shouldRestart(svc, failed) {
			return
		}
		if svc.RestartSec > 0 {
			select {
			case <-time.After(svc.RestartSec):
			case <-s.stopCh:
				return
			}
		}
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

// shutdown signals every supervised process and waits for them to
// exit, with a per-service kill timeout. Returns the container exit
// code (0 on graceful shutdown, 1 if any service was force-killed).
func (s *Supervisor) shutdown() int {
	const grace = 5 * time.Second
	s.mu.Lock()
	procs := make([]*serviceState, 0, len(s.services))
	for _, st := range s.services {
		procs = append(procs, st)
	}
	bounds := make([]*socketact.Bound, 0, len(s.bounds))
	for _, b := range s.bounds {
		bounds = append(bounds, b)
	}
	s.mu.Unlock()

	for i := len(procs) - 1; i >= 0; i-- {
		p := procs[i]
		if p.exited || p.pid <= 0 {
			continue
		}
		// Polite first pass: SIGTERM the immediate child so anything
		// with a graceful-shutdown handler gets a chance to run.
		// Force-kill via cgroup.kill (or PGID fallback) lands after
		// the grace window if needed.
		_ = syscall.Kill(p.pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	exit := 0
	for _, p := range procs {
		dl := time.Until(deadline)
		if dl < 0 {
			dl = 0
		}
		end := time.Now().Add(dl)
		for time.Now().Before(end) {
			s.mu.Lock()
			done := p.exited
			s.mu.Unlock()
			if done {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		s.mu.Lock()
		done := p.exited
		s.mu.Unlock()
		if !done {
			// Force-kill: cgroup.kill is atomic and reaches every
			// descendant regardless of PID reuse. Without cgroup-v2,
			// fall back to PID + PGID SIGKILL (still racy per the original
			// surprise #3, but the best we have on cgroup-v1 hosts).
			if s.cgroup.Available() {
				_ = s.cgroup.Kill(p.name)
			} else if p.pid > 0 {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
				_ = killGroup(p.pid, syscall.SIGKILL)
			}
			log.Printf("shutdown: %s force-killed (grace expired)", p.name)
			exit = 1
		} else if s.cgroup.Available() {
			// Even on graceful exit, sweep the cgroup so leftover
			// orphans (dbus-daemon etc.) don't outlive the
			// container's reverse-shutdown.
			_ = s.cgroup.Kill(p.name)
		}
		if s.cgroup.Available() {
			_ = s.cgroup.Remove(p.name)
		}
	}
	for _, b := range bounds {
		b.Close()
	}
	if s.tracer != nil {
		s.tracer.Event("reverse_shutdown_done", map[string]any{"exit": exit})
	}
	return exit
}

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

// setEnv replaces (or appends) "key=value" in env. Used to overwrite
// HOME / USER / LOGNAME on privilege drop so the dropped-priv child
// sees the resolved identity, not whatever container-init inherited
// as PID 1.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
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
