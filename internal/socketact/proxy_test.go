package socketact

import (
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestProxyToCopiesBothDirections verifies that bytes written by the client
// reach the backend and echoed bytes return to the client.
func TestProxyToCopiesBothDirections(t *testing.T) {
	// Echo backend.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); io.Copy(c, c) }(c)
		}
	}()

	// In-process client <-> proxy pair.
	client, proxySide := tcpPair(t)

	proxyErr := make(chan error, 1)
	go func() {
		err := ProxyTo(proxySide, "tcp", backend.Addr().String(),
			100*time.Millisecond, 2*time.Second)
		proxySide.Close()
		proxyErr <- err
	}()

	payload := []byte("hello proxy")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got := make([]byte, len(payload))
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echoed %q, want %q", got, payload)
	}

	client.Close()
	select {
	case err := <-proxyErr:
		if err != nil {
			t.Errorf("ProxyTo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ProxyTo did not return after client closed")
	}
}

// TestDialWithRetrySucceeds verifies that dialWithRetry connects when the
// backend starts partway through the total timeout window.
func TestDialWithRetrySucceeds(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "test.sock")

	go func() {
		time.Sleep(80 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		defer ln.Close()
		c, _ := ln.Accept()
		if c != nil {
			c.Close()
		}
	}()

	conn, err := dialWithRetry("unix", sock, 50*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("dialWithRetry: %v", err)
	}
	conn.Close()
}

// TestDialWithRetryTimesOut verifies that dialWithRetry returns an error
// after the total timeout when the backend never starts.
func TestDialWithRetryTimesOut(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "never.sock")

	start := time.Now()
	_, err := dialWithRetry("unix", sock, 25*time.Millisecond, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v, want < 500ms", elapsed)
	}
}

func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcpPair listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("tcpPair dial: %v", err)
	}
	return client, <-accepted
}
