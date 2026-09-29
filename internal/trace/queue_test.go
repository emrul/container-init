//go:build unix

package trace

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// stalledTracer returns a tracer writing into a FIFO that nobody reads
// until drain is called, so its writer goroutine blocks once the pipe
// buffer fills. drain copies everything written to out.
func stalledTracer(t *testing.T) (tr *Tracer, drain func(out io.Writer) <-chan struct{}) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "trace")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envEnable, "1")
	t.Setenv(envPath, fifo)
	tr = New()
	started := false
	drain = func(out io.Writer) <-chan struct{} {
		done := make(chan struct{})
		started = true
		go func() { io.Copy(out, r); r.Close(); close(done) }()
		return done
	}
	t.Cleanup(func() {
		if !started {
			drain(io.Discard)
		}
	})
	return tr, drain
}

// fill emits until the pipe and the queue are both full: the writer is
// then blocked mid-write with Queue records behind it.
func fill(tr *Tracer) (emitted int) {
	for tr.Dropped() == 0 {
		tr.Event("x", map[string]any{"n": emitted})
		emitted++
	}
	return emitted
}

func TestEmitDoesNotWaitForStalledWriter(t *testing.T) {
	tr, _ := stalledTracer(t)
	start := time.Now()
	n := fill(tr)
	for i := 0; i < 1000; i++ {
		tr.Event("x", map[string]any{"n": n + i})
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("emitting %d records into a stalled tracer took %v", n+1000, el)
	}
	if got := tr.Dropped(); got < 1000 {
		t.Errorf("Dropped() = %d, want >= 1000 once the queue is full", got)
	}
}

func TestCloseBoundedByDeadlineWithStalledWriter(t *testing.T) {
	tr, _ := stalledTracer(t)
	fill(tr)
	tr.SetDeadline(time.Now().Add(100 * time.Millisecond))
	for i, limit := range []time.Duration{400 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond} {
		start := time.Now()
		tr.Close()
		if el := time.Since(start); el > limit {
			t.Errorf("Close #%d with a stalled writer took %v, want <= %v", i+1, el, limit)
		}
	}
	tr.Event("after_close", nil) // discarded; must not panic or block
}

func TestCloseWithoutDeadlineUsesCloseWait(t *testing.T) {
	tr, _ := stalledTracer(t)
	fill(tr)
	start := time.Now()
	tr.Close()
	if el := time.Since(start); el < CloseWait/2 || el > CloseWait+time.Second {
		t.Errorf("Close with a stalled writer and no deadline took %v, want about %v", el, CloseWait)
	}
}

// Every record is either written or counted, and the count reaches the
// stream once the writer catches up.
func TestDroppedRecordsAreAccounted(t *testing.T) {
	tr, drain := stalledTracer(t)
	n := fill(tr)
	for i := 0; i < 500; i++ {
		tr.Event("x", map[string]any{"n": n + i})
	}
	n += 500
	pr, pw := io.Pipe()
	drained := drain(pw)
	type result struct {
		written int
		total   uint64
		err     error
	}
	res := make(chan result, 1)
	go func() {
		var r result
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				r.err = err
				break
			}
			switch rec["phase"] {
			case "x":
				r.written++
			case "trace_dropped":
				r.total = uint64(rec["total"].(float64))
			}
		}
		io.Copy(io.Discard, pr)
		res <- r
	}()
	tr.SetDeadline(time.Now().Add(5 * time.Second))
	tr.Close()
	<-drained
	pw.Close()
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	dropped := tr.Dropped()
	if dropped == 0 {
		t.Fatal("no records were dropped; the queue never filled")
	}
	if r.total != dropped {
		t.Errorf("trace_dropped total = %d, Dropped() = %d", r.total, dropped)
	}
	if uint64(r.written)+dropped != uint64(n) {
		t.Errorf("written %d + dropped %d != emitted %d", r.written, dropped, n)
	}
}
