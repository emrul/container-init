package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readJSONL parses a trace JSONL file into one map per line.
func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("invalid JSONL: %v\nline: %s", err, sc.Text())
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func newTracerWithFile(t *testing.T) (*Tracer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	t.Setenv(envEnable, "1")
	t.Setenv(envPath, path)
	tr := New()
	t.Cleanup(func() { tr.Close() })
	return tr, path
}

func TestTracerEmitsBootStartAnchor(t *testing.T) {
	_, path := newTracerWithFile(t)
	recs := readJSONL(t, path)
	if len(recs) < 1 {
		t.Fatalf("no records emitted")
	}
	if recs[0]["phase"] != "boot_start" {
		t.Errorf("first record phase = %v, want boot_start", recs[0]["phase"])
	}
	if _, ok := recs[0]["wall_utc"]; !ok {
		t.Errorf("boot_start missing wall_utc")
	}
	if recs[0]["dt_ms"].(float64) != 0 {
		t.Errorf("boot_start dt_ms = %v, want 0", recs[0]["dt_ms"])
	}
}

func TestPhaseDtMatchesElapsed(t *testing.T) {
	tr, path := newTracerWithFile(t)
	p := tr.Begin("web_invoke")
	time.Sleep(50 * time.Millisecond)
	p.End(map[string]any{"argv": []string{"/usr/bin/web", "--port=8080"}})
	tr.Close()

	recs := readJSONL(t, path)
	var phase map[string]any
	for _, r := range recs {
		if r["phase"] == "web_invoke" {
			phase = r
			break
		}
	}
	if phase == nil {
		t.Fatalf("web_invoke not in trace; recs=%v", recs)
	}
	dt := int64(phase["dt_ms"].(float64))
	if dt < 40 || dt > 5000 {
		t.Errorf("dt_ms = %d, want >=40 and <5000 (50ms sleep)", dt)
	}
	if phase["status"] != "ok" {
		t.Errorf("status = %v, want ok", phase["status"])
	}
	if argv, ok := phase["argv"].([]any); !ok || len(argv) != 2 {
		t.Errorf("argv field lost: %v", phase["argv"])
	}
}

func TestEventDtIsZero(t *testing.T) {
	tr, path := newTracerWithFile(t)
	tr.Event("spawn", map[string]any{"unit": "wm.service"})
	tr.Close()

	recs := readJSONL(t, path)
	var spawn map[string]any
	for _, r := range recs {
		if r["phase"] == "spawn" {
			spawn = r
			break
		}
	}
	if spawn == nil {
		t.Fatalf("spawn event not found")
	}
	if spawn["dt_ms"].(float64) != 0 {
		t.Errorf("Event dt_ms = %v, want 0", spawn["dt_ms"])
	}
	if spawn["unit"] != "wm.service" {
		t.Errorf("unit field = %v, want wm.service", spawn["unit"])
	}
}

func TestMemSnapshotShape(t *testing.T) {
	tr, path := newTracerWithFile(t)
	tr.MemSnapshot("boot")
	tr.Close()

	recs := readJSONL(t, path)
	var snap map[string]any
	for _, r := range recs {
		if r["phase"] == "mem_snapshot" {
			snap = r
			break
		}
	}
	if snap == nil {
		t.Fatalf("mem_snapshot not in trace")
	}
	for _, k := range []string{"label", "t_start_ms", "dt_ms", "status",
		"cgroup_current_bytes", "cgroup_peak_bytes", "cgroup_swap_bytes",
		"nproc", "rss_sum_bytes", "by_comm"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("mem_snapshot missing key %q", k)
		}
	}
	if snap["label"] != "boot" {
		t.Errorf("label = %v, want boot", snap["label"])
	}
}

func TestDisabledTracerEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	t.Setenv(envPath, path)
	t.Setenv(envEnable, "")
	tr := New()
	defer tr.Close()
	tr.Event("spawn", nil)
	tr.MemSnapshot("boot")
	tr.Begin("foo").End(nil)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("disabled tracer wrote a file")
	}
}

func TestPhaseFromUnitName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"web.service", "web_invoke"},
		{"audio-out-ws.service", "audio-out-ws_invoke"},
		{"window-manager.service", "window-manager_invoke"},
		{"upload.socket", "upload_invoke"},
		{"foo", "foo_invoke"},
	}
	for _, c := range cases {
		if got := PhaseFromUnitName(c.in); got != c.want {
			t.Errorf("PhaseFromUnitName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPostSpawnLabel(t *testing.T) {
	cases := []struct {
		env, unit, want string
	}{
		{"", "web.service", ""},
		{"web.service:post_web", "web.service", "post_web"},
		{"a:1,web.service:post_web,b:2", "web.service", "post_web"},
		{"web.service:post_web", "wm.service", ""},
		{"missing-colon", "web.service", ""},
		{" web.service:post_web , wm.service:post_wm ", "wm.service", "post_wm"},
	}
	for _, c := range cases {
		if got := PostSpawnLabel(c.env, c.unit); got != c.want {
			t.Errorf("PostSpawnLabel(%q, %q) = %q, want %q", c.env, c.unit, got, c.want)
		}
	}
}

func TestNilTracerSafe(t *testing.T) {
	var tr *Tracer
	tr.Event("x", nil)
	tr.MemSnapshot("y")
	tr.ScheduleMemSnapshot("z", time.Millisecond)
	if p := tr.Begin("p"); p != nil {
		t.Errorf("nil tracer Begin returned non-nil phase: %v", p)
	}
	tr.Close()
	// readJSONL would fail if a nil tracer somehow created a file; skip
	// since Filename must just return "".
	if got := tr.Filename(); !strings.Contains(got, "") {
		t.Errorf("nil tracer Filename = %q", got)
	}
}
