package systemd1shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// wellKnownName is the bus name real systemd owns. RequestName'ing it
// is what makes systemd-run --user / busctl --user etc. discover us.
const wellKnownName = "org.freedesktop.systemd1"

// dialRetry bounds how often we retry dialing a bus address whose
// socket isn't there yet (typical case: user-session dbus-daemon hasn't
// started). It's intentionally chatty (every 2s) because PID 1 boots
// before the user session does and we want to attach soon after.
const dialRetry = 2 * time.Second

// Shim is a running compatibility surface. Call Close (or cancel the
// context passed to Open) to tear it down -- every bus connection is
// closed and the per-bus goroutines exit.
type Shim struct {
	log    logger
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open dials every address, registers org.freedesktop.systemd1, and
// returns immediately. Errors are per-address and logged; we don't
// fail the whole Open just because one address (e.g. the user bus) is
// down right now -- we keep retrying.
//
// addresses is the user-facing slice from --systemd1-shim. Empty
// addresses are skipped; duplicates are deduped.
func Open(ctx context.Context, addresses []string, log logger) (*Shim, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	deduped := dedupe(addresses)
	if len(deduped) == 0 {
		return nil, errors.New("systemd1shim: no bus addresses configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Shim{log: log, cancel: cancel}
	for _, addr := range deduped {
		s.wg.Add(1)
		go s.serveBus(ctx, addr)
	}
	return s, nil
}

// Close cancels the context and waits for all bus goroutines to exit.
// Safe to call multiple times.
func (s *Shim) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// serveBus owns one bus address: dial-with-retry, RequestName, serve,
// reconnect on disconnect. Exits only when the context is cancelled.
func (s *Shim) serveBus(ctx context.Context, addr string) {
	defer s.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.dialAndServe(ctx, addr); err != nil {
			s.log("systemd1shim[%s]: %v", addr, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(dialRetry):
		}
	}
}

// dialAndServe runs one full attach attempt. Returns nil on graceful
// disconnect; an error is logged once before the outer loop retries.
func (s *Shim) dialAndServe(ctx context.Context, addr string) error {
	resolved, authOpts, err := resolveAddress(addr)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	connOpts := append([]dbus.ConnOption{}, authOpts...)
	connOpts = append(connOpts, dbus.WithContext(ctx))
	conn, err := dbus.Dial(resolved, connOpts...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	mgr := newManager(conn, func(format string, args ...any) {
		s.log("systemd1shim[%s]: "+format, append([]any{addr}, args...)...)
	})
	if err := conn.Export(mgr, managerPath, managerIface); err != nil {
		return fmt.Errorf("export manager: %w", err)
	}
	if err := conn.Export(introspect.Introspectable(managerIntrospectXML), managerPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return fmt.Errorf("export introspectable: %w", err)
	}

	reply, err := conn.RequestName(wellKnownName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request name: %w", err)
	}
	switch reply {
	case dbus.RequestNameReplyPrimaryOwner:
		s.log("systemd1shim[%s]: owning %s", addr, wellKnownName)
	case dbus.RequestNameReplyAlreadyOwner:
		// Shouldn't happen on a fresh conn, but harmless.
		s.log("systemd1shim[%s]: already owner of %s", addr, wellKnownName)
	default:
		// A real systemd (or another shim) holds the name. Don't fight
		// for it -- log once and keep this connection idle until the
		// peer drops or we're cancelled. We deliberately don't queue.
		s.log("systemd1shim[%s]: %s already owned (reply=%d) -- standing down", addr, wellKnownName, reply)
		<-ctx.Done()
		return nil
	}

	// Block until the connection closes (peer hangup, context cancel,
	// or a transport error). godbus cancels conn.Context() in all
	// three cases; we tell the parent-cancel case apart from a real
	// disconnect by re-checking ctx.Err().
	<-conn.Context().Done()
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("disconnected by peer")
}

// resolveAddress maps user-facing forms to a real D-Bus address and,
// when relevant, an AuthExternal option that asserts a specific UID.
//
//	system            -> the system bus
//	user              -> the calling uid's $XDG_RUNTIME_DIR/bus
//	user:UID          -> /run/user/UID/bus
//	user:env:VAR      -> /run/user/$VAR/bus  (e.g. user:env:KASM_OS_UID)
//	unix:path=...     -> verbatim
//	(everything else is passed through unchanged)
//
// The env form exists because container-init is typically PID 1 with
// no shell to expand variables in its argv. Kasm sets the workspace
// uid via KASM_OS_UID at container start, so the operator passes
// `--systemd1-shim=user:env:KASM_OS_UID` and the shim reads the var
// itself.
func resolveAddress(addr string) (string, []dbus.ConnOption, error) {
	switch {
	case addr == "system":
		sys := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
		if sys == "" {
			sys = "unix:path=/var/run/dbus/system_bus_socket"
		}
		return sys, nil, nil
	case addr == "user":
		uid := os.Getuid()
		return "unix:path=" + userBusPath(uid), nil, nil
	case strings.HasPrefix(addr, "user:env:"):
		varName := strings.TrimPrefix(addr, "user:env:")
		if varName == "" {
			return "", nil, fmt.Errorf("empty env var name in %q", addr)
		}
		val := os.Getenv(varName)
		if val == "" {
			return "", nil, fmt.Errorf("env var %s is unset or empty", varName)
		}
		uid, err := strconv.Atoi(val)
		if err != nil || uid < 0 {
			return "", nil, fmt.Errorf("env var %s = %q is not a valid uid", varName, val)
		}
		return "unix:path=" + userBusPath(uid), nil, nil
	case strings.HasPrefix(addr, "user:"):
		uidStr := strings.TrimPrefix(addr, "user:")
		uid, err := strconv.Atoi(uidStr)
		if err != nil || uid < 0 {
			return "", nil, fmt.Errorf("invalid uid in %q", addr)
		}
		return "unix:path=" + userBusPath(uid), nil, nil
	default:
		return addr, nil, nil
	}
}

// userBusPath returns the conventional per-uid user bus path. Prefer
// $XDG_RUNTIME_DIR when the request is for the calling uid (covers
// non-standard layouts on rootless setups); fall back to /run/user/UID
// for any other uid or when the env var is empty.
func userBusPath(uid int) string {
	if uid == os.Getuid() {
		if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
			return x + "/bus"
		}
	}
	return fmt.Sprintf("/run/user/%d/bus", uid)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
