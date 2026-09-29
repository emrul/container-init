//go:build linux

package supervisor

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emrul/container-init/unit"
)

// socketPair returns a socket in mode and the service it activates,
// both named name, listening on dir/sock.
func socketPair(t *testing.T, mode unit.ActivationMode, dir, name string) (sock, svc *unit.Unit) {
	t.Helper()
	sock = &unit.Unit{
		Name:           name + ".socket",
		Kind:           unit.KindSocket,
		ListenStream:   []unit.Listener{{Network: "unix", Address: filepath.Join(dir, "sock")}},
		ActivationMode: mode,
		Service:        name + ".service",
	}
	svc = &unit.Unit{
		Name: name + ".service",
		Kind: unit.KindService,
		Type: unit.TypeSimple,
	}
	if mode == unit.ActivationProxy {
		sock.ProxyTarget = filepath.Join(dir, "private")
	}
	return sock, svc
}

// helperService makes svc the socket helper, counting runs in count and
// exiting with code.
func helperService(t *testing.T, sock, svc *unit.Unit, count string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := "native"
	if sock.ActivationMode == unit.ActivationProxy {
		mode = sock.ProxyTarget
	}
	svc.ExecStart = []string{exe}
	svc.Environment = []string{
		socketHelperMode + "=" + mode,
		socketHelperCount + "=" + count,
		fmt.Sprintf("%s=%d", socketHelperExit, code),
	}
}

// request connects to the socket at path and returns the first line of
// the reply.
func request(t *testing.T, path string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read from %s: %v", path, err)
	}
	return line
}

// waitIdle waits until name's latest run has exited and the helper has
// counted n runs, then a little longer for the socket to listen again.
func waitIdle(t *testing.T, sup *Supervisor, name, count string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sup.mu.Lock()
		st := sup.services[name]
		exited := st != nil && st.exited
		sup.mu.Unlock()
		if exited && readCount(count) >= n {
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not exit after %d run(s) (count %d)", name, n, readCount(count))
}

// waitListening waits until the socket at path accepts connections,
// without connecting (a connection would trigger the service).
func waitListening(t *testing.T, sup *Supervisor, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sup.mu.Lock()
		_, ok := sup.bounds[name]
		sup.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never bound", name)
}

// waitClosed waits until nothing listens at path any more.
func waitClosed(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is still listening", path)
}

var activationModes = []unit.ActivationMode{unit.ActivationNative, unit.ActivationProxy}

// TestSocketRearmsAfterServiceStops: a service that exits and is not
// restarted -- cleanly, or failed under Restart=no -- is started again
// by the next connection, as in systemd.
func TestSocketRearmsAfterServiceStops(t *testing.T) {
	for _, mode := range activationModes {
		for _, code := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit-%d", mode, code), func(t *testing.T) {
				dir := t.TempDir()
				count := filepath.Join(dir, "count")
				sock, svc := socketPair(t, mode, dir, "rearm")
				helperService(t, sock, svc, count, code)
				sup := runUnits(t, sock, svc)
				path := sock.ListenStream[0].Address
				waitListening(t, sup, sock.Name)

				for i := 1; i <= 3; i++ {
					if got := request(t, path); got != "ok\n" {
						t.Fatalf("request %d = %q, want ok", i, got)
					}
					waitIdle(t, sup, svc.Name, count, i)
				}
				if n := readCount(count); n != 3 {
					t.Errorf("service ran %d time(s), want 3", n)
				}
			})
		}
	}
}

// TestSocketFailsWhenServiceHitsStartLimit: once the service it starts
// hits its start limit, the socket fails and stops listening.
func TestSocketFailsWhenServiceHitsStartLimit(t *testing.T) {
	for _, mode := range activationModes {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			sock, svc := socketPair(t, mode, dir, "limited")
			helperService(t, sock, svc, count, 0)
			svc.StartLimitBurst = 2
			svc.StartLimitIntervalSec = time.Minute
			sup := runUnits(t, sock, svc)
			path := sock.ListenStream[0].Address
			waitListening(t, sup, sock.Name)

			for i := 1; i <= 2; i++ {
				if got := request(t, path); got != "ok\n" {
					t.Fatalf("request %d = %q, want ok", i, got)
				}
				waitIdle(t, sup, svc.Name, count, i)
			}
			// The third connection is refused a service; the socket
			// closes.
			if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
				c.Close()
			}
			waitClosed(t, path)
			if n := readCount(count); n != 2 {
				t.Errorf("service ran %d time(s), want 2", n)
			}
			sup.mu.Lock()
			_, bound := sup.bounds[sock.Name]
			sup.mu.Unlock()
			if bound {
				t.Error("failed socket still in bounds")
			}
		})
	}
}

// TestSocketTriggerLimit: a service that exits at once without taking
// the connection re-triggers its socket; the trigger limit fails the
// socket instead of letting it spin, and the socket's OnFailure= fires.
func TestSocketTriggerLimit(t *testing.T) {
	for _, mode := range activationModes {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			mark := filepath.Join(dir, "onfailure")
			sock, svc := socketPair(t, mode, dir, "spin")
			svc.ExecStart = []string{"/bin/sh", "-c", countingScript(count, "exit 0")}
			sock.TriggerLimitBurst = 2
			sock.TriggerLimitIntervalSec = time.Minute
			sock.OnFailure = []string{"spin-failed.service"}
			drain := &unit.Unit{
				Name:      "spin-failed.service",
				Kind:      unit.KindService,
				Type:      unit.TypeOneshot,
				ExecStart: []string{"/bin/touch", mark},
			}
			sup := runUnits(t, sock, svc, drain)
			path := sock.ListenStream[0].Address
			waitListening(t, sup, sock.Name)

			// Native mode: the unaccepted connection keeps the listener
			// readable, so one connection re-triggers until the limit.
			// Proxy mode: each connection that finds no helper triggers.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
				if err != nil {
					break
				}
				c.Close()
				time.Sleep(100 * time.Millisecond)
			}
			waitClosed(t, path)
			if n := readCount(count); n != 2 {
				t.Errorf("service ran %d time(s), want 2 (the trigger limit)", n)
			}
			waitCount(t, count, 2)
			if _, err := os.Stat(mark); err != nil {
				t.Error("socket's OnFailure= did not fire")
			}
		})
	}
}

// TestSocketFailsWhenRequirementFails: a service that can never start
// because a unit it requires failed fails its socket, which closes
// instead of leaving clients queued or proxied to nothing.
func TestSocketFailsWhenRequirementFails(t *testing.T) {
	for _, mode := range activationModes {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			sock, svc := socketPair(t, mode, dir, "needy")
			helperService(t, sock, svc, count, 0)
			svc.Requires = []string{"broken.service"}
			svc.After = []string{"broken.service"}
			broken := &unit.Unit{
				Name:      "broken.service",
				Kind:      unit.KindService,
				Type:      unit.TypeOneshot,
				ExecStart: []string{"/bin/false"},
			}
			sup := runUnits(t, sock, svc, broken)
			path := sock.ListenStream[0].Address
			waitListening(t, sup, sock.Name)
			waitSettled(t, sup, broken.Name)

			if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
				c.Close()
			}
			waitClosed(t, path)
			if n := readCount(count); n != 0 {
				t.Errorf("service ran %d time(s), want 0", n)
			}
		})
	}
}
