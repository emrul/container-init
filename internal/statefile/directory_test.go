//go:build unix

package statefile

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A directory's parent is renamed about by another user while the
// state file's directories are made: no swap may turn the chmod of a
// new directory onto an existing file. All paths are private test
// fixtures; it needs no privileges.
func TestDirectorySwapMustNotChmodUnrelatedFile(t *testing.T) {
	base := t.TempDir()
	owned, victims := filepath.Join(base, "owned"), filepath.Join(base, "victims")
	for _, d := range []string{owned, victims} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3000; i++ {
		if err := os.WriteFile(filepath.Join(victims, fmt.Sprint(i)), []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "current")
	if err := os.Symlink(owned, link); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			for _, target := range []string{victims, owned} {
				select {
				case <-stop:
					return
				default:
				}
				_ = os.Symlink(target, link+"-next")
				_ = os.Rename(link+"-next", link)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 3000; i++ {
		if fd, _, err := openDir(filepath.Join(link, fmt.Sprint(i))); err == nil {
			unix.Close(fd)
		}
		victim := filepath.Join(victims, fmt.Sprint(i))
		fi, err := os.Stat(victim)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("directory replacement widened unrelated file from 0600 to %04o (attempt %d)", fi.Mode().Perm(), i)
		}
	}
}
