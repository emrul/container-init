//go:build unix

package trace

import (
	"testing"
	"time"
)

// SetDeadline documents that it also ends a flush already in progress.
func TestDeadlineShortensActiveClose(t *testing.T) {
	tr, _ := stalledTracer(t)
	fill(tr)
	done := make(chan struct{})
	go func() { tr.Close(); close(done) }()
	for {
		tr.mu.RLock()
		closed := tr.closed
		tr.mu.RUnlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Allow Close to enter its timer wait before changing the deadline.
	time.Sleep(20 * time.Millisecond)
	tr.SetDeadline(time.Now().Add(50 * time.Millisecond))
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Error("SetDeadline did not shorten the active Close flush")
		<-done
	}
}
