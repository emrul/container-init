//go:build unix

package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sample() *File {
	at := time.Date(2026, 9, 29, 17, 3, 12, 900_000_000, time.FixedZone("x", 3600))
	return &File{
		Version: Version,
		Written: Time{at},
		PID1:    PID1{Version: "v1.3.2", Started: Time{at}},
		Units: map[string]Unit{
			"a.service": {Type: "simple", Active: ActiveActive, Sub: SubRunning, Result: ResultSuccess, Runs: 1, Since: Time{at}},
			"a.socket":  {Type: TypeSocket, Active: ActiveActive, Sub: SubListening, Result: ResultSuccess, Runs: 1, Since: Time{at}, Service: "a.service"},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Units["a.socket"].Service != "a.service" || f.PID1.Version != "v1.3.2" {
		t.Errorf("round trip lost fields: %+v", f)
	}
	// RFC 3339, UTC, whole seconds.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"written": "2026-09-29T16:03:12Z"`) {
		t.Errorf("written not UTC whole seconds:\n%s", data)
	}
	if strings.Contains(string(data), `"never"`) || strings.Contains(string(data), `"sockets"`) {
		t.Errorf("empty optional fields written:\n%s", data)
	}
}

// The file is 0644 whatever the umask, and no temporary file is left.
func TestWriteModeAndNoLeftovers(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 0; i < 2; i++ { // the second replaces the first
		if _, err := Write(path, sample()); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %04o, want 0644", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the state file", len(entries))
	}
}

// A missing directory is made 0755 with its parents; an existing one is
// left alone.
func TestWriteDirectories(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	base := t.TempDir()
	path := filepath.Join(base, "run", "kasm", "state.json")
	created, err := Write(path, sample())
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Errorf("created %v, want run and run/kasm", created)
	}
	for _, d := range []string{filepath.Join(base, "run"), filepath.Join(base, "run", "kasm")} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %04o, want 0755", d, st.Mode().Perm())
		}
	}
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if created, err := Write(filepath.Join(private, "state.json"), sample()); err != nil || len(created) != 0 {
		t.Fatalf("Write into an existing directory: created %v, err %v", created, err)
	}
	if st, _ := os.Stat(private); st.Mode().Perm() != 0o700 {
		t.Errorf("existing directory's mode changed to %04o", st.Mode().Perm())
	}
}

// A failed write leaves no temporary file and the old file in place.
func TestWriteFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if _, err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be: the rename fails.
	bad := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(bad, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(bad, sample()); err == nil {
		t.Fatal("Write over a non-empty directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

func TestReadRejects(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"bad json":        "{",
		"unknown version": `{"version": 2, "written": "2026-09-29T17:00:00Z"}`,
		"bad time":        `{"version": 1, "written": "yesterday"}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); err == nil {
			t.Errorf("%s: Read accepted it", name)
		}
	}
	if _, err := Read(filepath.Join(dir, "absent")); err == nil {
		t.Error("Read of a missing file succeeded")
	}
}
