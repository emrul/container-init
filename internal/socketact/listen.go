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

// Perm is the ownership and mode of an AF_UNIX socket node, from
// [Socket] SocketUser= / SocketGroup= / SocketMode=. UID and GID of -1
// leave the owner as bound (container-init's own). Mode applies only
// when ModeSet, so an explicit 0000 stays 0000; otherwise the node
// gets systemd's default, 0666. TCP listeners ignore it.
type Perm struct {
	Mode     os.FileMode
	ModeSet  bool
	UID, GID int
}

// DefaultSocketMode is systemd's SocketMode= default.
const DefaultSocketMode os.FileMode = 0o666

// Bind opens the listener described by l. The returned *os.File is
// kept by container-init; native mode passes it as fd 3 to the child,
// proxy mode keeps it and accepts on it directly.
func Bind(l unit.Listener, p Perm) (*Bound, error) {
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
		b := &Bound{Listener: l, File: f, listener: ln}
		// The node is created with the umask applied; set what the
		// unit asked for. A client can connect in the moment before
		// the chmod, with the umask's mode (0755 under the usual 022,
		// which grants connect to nobody but the owner).
		if err := applyPerm(l.Address, p); err != nil {
			b.Close()
			return nil, fmt.Errorf("listen unix %s: %w", l.Address, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("unsupported network %q", l.Network)
}

func applyPerm(path string, p Perm) error {
	if p.UID != -1 || p.GID != -1 {
		if err := os.Lchown(path, p.UID, p.GID); err != nil {
			return err
		}
	}
	mode := DefaultSocketMode
	if p.ModeSet {
		mode = p.Mode
	}
	return os.Chmod(path, mode)
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
