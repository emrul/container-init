//go:build linux

package supervisor

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/unit"
)

// A trace destination that stops accepting writes must not hold up
// shutdown: shutdown itself, and main's final flush after it (the
// tracer's Close), return within the stop timeout -- not merely the
// units' processes being stopped.
func TestShutdownReturnsWithBlockedTrace(t *testing.T) {
	live := &unit.Unit{Name: "live-audit.service", Kind: unit.KindService, Type: unit.TypeSimple,
		ExecStart: []string{"/bin/sleep", "60"}}
	s := auditSupervisor(t, live)
	s.SetStopTimeout(100 * time.Millisecond)

	// A FIFO nobody reads until the end: once its pipe buffer is full
	// the tracer's writer is stuck in write(2).
	fifo := filepath.Join(t.TempDir(), "trace")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_INIT_TRACE", "1")
	t.Setenv("CONTAINER_INIT_TRACE_FILE", fifo)
	s.tracer = trace.New()
	drained := make(chan struct{})
	defer func() {
		go func() { io.Copy(io.Discard, r); r.Close(); close(drained) }()
		<-drained
	}()

	liveDone, spawned := make(chan struct{}), make(chan struct{})
	go func() { s.spawnAndWait(live, nil, func() { close(spawned) }); close(liveDone) }()
	select {
	case <-spawned:
	case <-time.After(3 * time.Second):
		t.Fatal("live service did not start")
	}
	// Fill the pipe and then the queue; a drop proves the writer is
	// stuck.
	for i := 0; s.tracer.Dropped() == 0; i++ {
		if i > 1_000_000 {
			t.Fatal("trace writer never stalled; the fault was not injected")
		}
		s.tracer.Event("filler", map[string]any{"i": i})
	}

	start := time.Now()
	s.Stop()
	stopped := make(chan struct{})
	go func() {
		s.shutdown()
		s.tracer.Close()
		s.tracer.Close() // a deferred or repeated Close is bounded too
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(350 * time.Millisecond):
		t.Errorf("shutdown and the final trace flush took over 350ms with a blocked trace destination (stop timeout 100ms)")
		<-stopped
	}
	t.Logf("shutdown returned in %v", time.Since(start))
	<-liveDone
}
