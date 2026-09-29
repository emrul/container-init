package socketact

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/emrul/container-init/unit"
)

// A listener numbered past select(2)'s 1024-descriptor limit must still
// be waited on, and WaitReadable must stop once the listener is closed.
func TestWaitReadableHighDescriptor(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "sock")
	b, err := Bind(unit.Listener{Network: "unix", Address: addr}, Perm{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil || lim.Cur < 1200 {
		t.Skipf("RLIMIT_NOFILE too low for the test (%d, %v)", lim.Cur, err)
	}
	high, err := unix.FcntlInt(b.File.Fd(), unix.F_DUPFD_CLOEXEC, 1100)
	if err != nil {
		t.Fatal(err)
	}
	b.File.Close()
	b.File = os.NewFile(uintptr(high), "high")

	woke := make(chan error, 1)
	go func() { woke <- b.WaitReadable(nil) }()
	c, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("WaitReadable: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitReadable did not see the connection")
	}

	// Closed while waiting: the next poll slice notices. Accept the
	// queued connection first so the listener is idle.
	conn, err := b.Accept()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	go func() { woke <- b.WaitReadable(nil) }()
	time.Sleep(20 * time.Millisecond)
	b.Close()
	select {
	case err := <-woke:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("WaitReadable after Close = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitReadable did not return after Close")
	}
}
