package supervisor

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Environment that turns the test binary into a socket-activated
// service: it serves one connection ("ok\n"), counts the run in
// socketHelperCount, and exits with socketHelperExit.
const (
	socketHelperMode  = "SUPERVISOR_TEST_SOCKET_HELPER" // "native" or a proxy endpoint path
	socketHelperCount = "SUPERVISOR_TEST_SOCKET_COUNT"
	socketHelperExit  = "SUPERVISOR_TEST_SOCKET_EXIT"
)

// socketHelperMain runs the helper when the environment asks for it,
// and returns otherwise.
func socketHelperMain() {
	mode := os.Getenv(socketHelperMode)
	if mode == "" {
		return
	}
	var ln net.Listener
	var err error
	if mode == "native" {
		ln, err = net.FileListener(os.NewFile(3, "listener"))
	} else {
		_ = os.Remove(mode)
		ln, err = net.Listen("unix", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "socket helper:", err)
		os.Exit(2)
	}
	if c, err := ln.Accept(); err == nil {
		fmt.Fprintln(c, "ok")
		c.Close()
	}
	ln.Close()
	if f := os.Getenv(socketHelperCount); f != "" {
		n := 0
		if data, err := os.ReadFile(f); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		_ = os.WriteFile(f, []byte(strconv.Itoa(n+1)), 0o644)
	}
	if os.Getenv(socketHelperExit) == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}
