//go:build unix

package socketact

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/emrul/container-init/unit"
)

// shortTempDir is a temporary directory short enough for a staged
// socket path to fit sockaddr_un on systems without /proc/self/fd
// (macOS's own temp directories are too long).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// socketUnit parses a .socket unit listening on address with the given
// extra [Socket] lines, and returns its listener and the Perm the unit
// asks for.
func socketUnit(t *testing.T, address, extra string) (unit.Listener, Perm) {
	t.Helper()
	config := filepath.Join(shortTempDir(t), "s.socket")
	body := "[Socket]\nListenStream=" + address + "\n" + extra
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _, err := unit.LoadFile(config, unit.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return u.ListenStream[0], Perm{Mode: u.SocketMode, ModeSet: u.SocketModeSet, UID: -1, GID: -1}
}

// An omitted SocketMode= gets systemd's 0666; an explicit one, 0000
// included, is applied exactly.
func TestBindSocketMode(t *testing.T) {
	cases := []struct {
		name, extra string
		want        os.FileMode
	}{
		{"omitted", "", 0o666},
		{"explicit 0000", "SocketMode=0000\n", 0},
		{"explicit 0600", "SocketMode=0600\n", 0o600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address := filepath.Join(shortTempDir(t), "sock")
			l, p := socketUnit(t, address, tc.extra)
			b, err := Bind(l, p)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			st, err := os.Lstat(address)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Mode().Perm(); got != tc.want {
				t.Errorf("mode = %04o, want %04o", got, tc.want)
			}
		})
	}
}

// The public path does not exist until the socket's owner and mode are
// final, even under umask 000 -- where a bind-then-chmod would expose a
// world-connectable 0777 node -- and it is connectable once published.
func TestBindPublishesOnlyAfterPermissions(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := shortTempDir(t)
	address := filepath.Join(dir, "sock")
	l, p := socketUnit(t, address, "SocketMode=0600\n")
	wantUID, wantGID := os.Geteuid(), ownGroup(t, dir)
	if os.Geteuid() == 0 {
		p.UID, p.GID = 4242, 4343
		wantUID, wantGID = 4242, 4343
	}
	paused := false
	beforePublish = func(staged string) {
		paused = true
		if _, err := os.Lstat(address); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("public path exists before publication (err %v)", err)
		}
		if c, err := net.Dial("unix", address); err == nil {
			c.Close()
			t.Error("connected to the public path before publication")
		}
		st, err := os.Stat(filepath.Dir(staged))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != 0o700 {
			t.Errorf("staging directory mode = %04o, want 0700", got)
		}
		checkNode(t, staged, 0o600, wantUID, wantGID)
	}
	defer func() { beforePublish = nil }()
	b, err := Bind(l, p)
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("Bind did not stage the socket")
	}
	checkNode(t, address, 0o600, wantUID, wantGID)
	c, err := net.Dial("unix", address)
	if err != nil {
		t.Fatalf("connect after publication: %v", err)
	}
	c.Close()
	if conn, err := b.Accept(); err != nil {
		t.Errorf("accept: %v", err)
	} else {
		conn.Close()
	}
	checkNoStaging(t, dir)
	b.Close()
	if _, err := os.Lstat(address); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket node left behind after Close (err %v)", err)
	}
}

func checkNode(t *testing.T, path string, mode os.FileMode, uid, gid int) {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Type() != fs.ModeSocket {
		t.Errorf("%s is %v, want a socket", path, st.Mode().Type())
	}
	if got := st.Mode().Perm(); got != mode {
		t.Errorf("%s mode = %04o, want %04o", path, got, mode)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if int(sys.Uid) != uid || int(sys.Gid) != gid {
		t.Errorf("%s owner = %d:%d, want %d:%d", path, sys.Uid, sys.Gid, uid, gid)
	}
}

// ownGroup is the group a node created in dir gets: the creator's on
// Linux, the directory's own under BSD semantics (macOS).
func ownGroup(t *testing.T, dir string) int {
	t.Helper()
	if runtime.GOOS == "linux" {
		return os.Getegid()
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return int(st.Sys().(*syscall.Stat_t).Gid)
}

func checkNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ci-") {
			t.Errorf("staging directory %s left behind", e.Name())
		}
	}
}

// A socket that cannot be published fails, leaving nothing behind: here
// the public path is a non-empty directory the rename cannot replace.
func TestBindFailureCleansUp(t *testing.T) {
	dir := shortTempDir(t)
	address := filepath.Join(dir, "sock")
	if err := os.MkdirAll(filepath.Join(address, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, p := socketUnit(t, address, "")
	if b, err := Bind(l, p); err == nil {
		b.Close()
		t.Fatal("Bind succeeded over a non-empty directory")
	}
	checkNoStaging(t, dir)
	if _, err := os.Stat(filepath.Join(address, "occupied")); err != nil {
		t.Errorf("the occupying directory was disturbed: %v", err)
	}
}

// A stale socket from a previous run is replaced.
func TestBindReplacesStaleSocket(t *testing.T) {
	address := filepath.Join(shortTempDir(t), "sock")
	stale, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	l, p := socketUnit(t, address, "")
	b, err := Bind(l, p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// A path that fits sockaddr_un but not once staging lengthens it is
// bound through /proc/self/fd.
func TestBindLongPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc/self/fd")
	}
	dir := shortTempDir(t)
	// 100 bytes: under Linux's 108, over it with the staging suffix.
	pad := 100 - len(dir) - len("/") - len("/sock")
	if pad < 1 {
		t.Skipf("temp dir %s too long", dir)
	}
	dir = filepath.Join(dir, strings.Repeat("d", pad))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(dir, "sock")
	l, p := socketUnit(t, address, "SocketMode=0600\n")
	b, err := Bind(l, p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	checkNode(t, address, 0o600, os.Geteuid(), ownGroup(t, dir))
	c, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
