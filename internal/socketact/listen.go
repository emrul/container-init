package socketact

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/emrul/container-init/unit"
)

// Bound holds the OS-level listener for a single .socket unit, kept
// alive in container-init for the lifetime of the container so it can
// be re-passed across service restarts.
type Bound struct {
	Listener unit.Listener
	File     *os.File // owns the bound fd; closed on shutdown or when the socket fails
	listener net.Listener
	// mu orders Close against WaitReadable's use of the fd: a poll
	// never runs on a descriptor that has been closed (and possibly
	// reused for something else).
	mu     sync.Mutex
	closed bool
}

// Bind opens the listener described by l. The returned *os.File is
// kept by container-init; native mode passes it as fd 3 to the child,
// proxy mode keeps it and accepts on it directly.
func Bind(l unit.Listener) (*Bound, error) {
	switch l.Network {
	case "tcp":
		ln, err := net.Listen("tcp", l.Address)
		if err != nil {
			return nil, fmt.Errorf("listen tcp %s: %w", l.Address, err)
		}
		f, err := ln.(*net.TCPListener).File()
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("listen tcp %s: file: %w", l.Address, err)
		}
		return &Bound{Listener: l, File: f, listener: ln}, nil
	case "unix":
		// Remove a stale socket from a previous run; absent file is
		// not an error.
		_ = os.Remove(l.Address)
		ln, err := net.Listen("unix", l.Address)
		if err != nil {
			return nil, fmt.Errorf("listen unix %s: %w", l.Address, err)
		}
		f, err := ln.(*net.UnixListener).File()
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("listen unix %s: file: %w", l.Address, err)
		}
		// AF_UNIX listeners are typically chmod 0660 by default. Open
		// up so the original helper (running as the same UID as
		// container-init) can be reached from any process inside the
		// container; production policy belongs in [Socket]
		// SocketMode= when Phase 4 widens the directive set.
		_ = os.Chmod(l.Address, 0o666)
		return &Bound{Listener: l, File: f, listener: ln}, nil
	}
	return nil, fmt.Errorf("unsupported network %q", l.Network)
}

// Close releases the bound listener, on reverse shutdown or when the
// socket fails. Only the first call acts, so a later one cannot remove
// a socket something else has since bound at the same path. It waits
// for a WaitReadable poll in progress to finish its slice.
func (b *Bound) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	if b.listener != nil {
		_ = b.listener.Close()
	}
	if b.File != nil {
		_ = b.File.Close()
	}
	if b.Listener.Network == "unix" {
		_ = os.Remove(b.Listener.Address)
	}
}

// ErrClosed is what WaitReadable returns once the listener is closed.
var ErrClosed = errors.New("socket closed")

// pollSlice bounds how long one poll holds the listener: Close, and a
// closed stop channel, are noticed within it.
const pollSlice = 100 * time.Millisecond

// WaitReadable blocks until the listener has a connection waiting, stop
// is closed, or the listener is closed. It polls with poll(2), which,
// unlike select(2), takes descriptors of any number.
func (b *Bound) WaitReadable(stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return errors.New("stopped")
		default:
		}
		ready, err := b.pollOnce()
		if err != nil || ready {
			return err
		}
	}
}

func (b *Bound) pollOnce() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false, ErrClosed
	}
	fds := []unix.PollFd{{Fd: int32(b.File.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(pollSlice/time.Millisecond))
	if err == unix.EINTR {
		return false, nil // Go's preemption signals interrupt poll routinely
	}
	if err != nil {
		return false, err
	}
	return n > 0 && fds[0].Revents != 0, nil
}

// Accept returns the next connection on the bound listener. Used by
// proxy mode and by the activation waiter to detect first-connect.
func (b *Bound) Accept() (net.Conn, error) {
	return b.listener.Accept()
}

// Listener returns the underlying net.Listener (for proxy mode).
func (b *Bound) Net() net.Listener { return b.listener }
