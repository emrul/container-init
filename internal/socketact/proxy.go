package socketact

import (
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Proxy copies bytes between an accepted public connection and the
// helper's private endpoint. ProxyTo dials target with retries (the
// helper may still be cold-starting), then runs two io.Copy goroutines
// until either side closes.
//
// Returns when both halves have finished. Caller is responsible for
// closing the public conn.
func ProxyTo(public net.Conn, network, target string, dialTimeout, totalTimeout time.Duration) error {
	private, err := dialWithRetry(network, target, dialTimeout, totalTimeout)
	if err != nil {
		return err
	}
	defer private.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(private, public)
		// Half-close the private write side so the helper sees EOF.
		if c, ok := private.(closeWriter); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(public, private)
		if c, ok := public.(closeWriter); ok {
			_ = c.CloseWrite()
		}
	}()
	wg.Wait()
	return nil
}

type closeWriter interface{ CloseWrite() error }

// dialWithRetry retries until the helper's private endpoint becomes
// reachable or totalTimeout elapses. Backoff starts at 25ms and caps
// at 200ms — short because the helper is local and we want first-byte
// latency low.
func dialWithRetry(network, target string, dialTimeout, totalTimeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(totalTimeout)
	backoff := 25 * time.Millisecond
	var lastErr error
	for {
		c, err := net.DialTimeout(network, target, dialTimeout)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if time.Now().Add(backoff).After(deadline) {
			return nil, lastErr
		}
		time.Sleep(backoff)
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// Log is the per-proxy stderr line for a single accepted connection.
// Caller invokes this so the supervisor's prefix is consistent.
func Log(unitName, public, private string, err error) {
	if err != nil {
		log.Printf("socket %s proxy %s -> %s error: %v", unitName, public, private, err)
		return
	}
	log.Printf("socket %s proxy %s -> %s done", unitName, public, private)
}
