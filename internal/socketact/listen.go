package socketact

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
		return bindUnix(l, p)
	}
	return nil, fmt.Errorf("unsupported network %q", l.Network)
}

// bindUnix binds l's socket in a private staging directory beside its
// path, sets the owner and mode p asks for, and only then renames it
// into place. The public path never exists with any other permissions,
// so no client can connect before they apply (a bind-then-chmod would
// leave the umask's mode reachable meanwhile). The staging directory
// is on the same filesystem, so the rename is atomic; it also replaces
// a stale socket left by a previous run. On any failure nothing is
// left at either path.
func bindUnix(l unit.Listener, p Perm) (*Bound, error) {
	dst := l.Address
	fail := func(err error) (*Bound, error) {
		return nil, fmt.Errorf("listen unix %s: %w", dst, err)
	}
	// MkdirTemp creates it 0700, whatever the umask.
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".ci-")
	if err != nil {
		return fail(fmt.Errorf("staging: %w", err))
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "s")
	addr, closeDir, err := bindAddr(stage, staged)
	if err != nil {
		return fail(err)
	}
	defer closeDir()
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return fail(err)
	}
	ul := ln.(*net.UnixListener)
	// Close must not unlink the staged name: once published, the node
	// lives at dst, which Bound.Close removes.
	ul.SetUnlinkOnClose(false)
	f, err := ul.File()
	if err != nil {
		ln.Close()
		return fail(fmt.Errorf("file: %w", err))
	}
	b := &Bound{Listener: l, File: f, listener: ln}
	if err := applyPerm(staged, p); err != nil {
		b.closeStaged()
		return fail(err)
	}
	if beforePublish != nil {
		beforePublish(staged)
	}
	if err := os.Rename(staged, dst); err != nil {
		b.closeStaged()
		return fail(fmt.Errorf("publish: %w", err))
	}
	return b, nil
}

// beforePublish, when set by a test, runs between a Unix socket's
// permissions being applied and its rename into place.
var beforePublish func(staged string)

// closeStaged releases a listener that was never published; its staged
// node goes with the staging directory.
func (b *Bound) closeStaged() {
	b.closed = true
	_ = b.listener.Close()
	_ = b.File.Close()
}

// bindAddr returns the address to bind staged at. A sockaddr_un path
// is short (108 bytes on Linux), and staging lengthens it; when it no
// longer fits, bind through the staging directory's descriptor under
// /proc/self/fd instead, which names the same directory in a few bytes.
func bindAddr(stage, staged string) (string, func(), error) {
	if len(staged) < len(unix.RawSockaddrUnix{}.Path) {
		return staged, func() {}, nil
	}
	d, err := os.Open(stage)
	if err != nil {
		return "", nil, fmt.Errorf("staging: %w", err)
	}
	addr := fmt.Sprintf("/proc/self/fd/%d/s", d.Fd())
	if _, err := os.Stat(filepath.Dir(addr)); err != nil {
		d.Close()
		return "", nil, fmt.Errorf("path too long to stage (%d bytes) and no /proc/self/fd", len(staged))
	}
	return addr, func() { d.Close() }, nil
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
