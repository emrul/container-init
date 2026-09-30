//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

func TestShouldRestart(t *testing.T) {
	cases := []struct {
		restart unit.RestartPolicy
		failed  bool
		want    bool
	}{
		{unit.RestartNo, false, false},
		{unit.RestartNo, true, false},
		{unit.RestartAlways, false, true},
		{unit.RestartAlways, true, true},
		{unit.RestartOnFailure, false, false},
		{unit.RestartOnFailure, true, true},
	}
	for _, tc := range cases {
		u := &unit.Unit{Restart: tc.restart}
		if got := shouldRestart(u, tc.failed); got != tc.want {
			t.Errorf("shouldRestart(Restart=%v, failed=%v) = %v, want %v",
				tc.restart, tc.failed, got, tc.want)
		}
	}
}

func TestSplitProxyTarget(t *testing.T) {
	cases := []struct{ input, network, address string }{
		{"127.0.0.1:8080", "tcp", "127.0.0.1:8080"},
		{"tcp:127.0.0.1:8080", "tcp", "127.0.0.1:8080"},
		{"unix:/run/app.sock", "unix", "/run/app.sock"},
		{"/run/app.sock", "unix", "/run/app.sock"},
	}
	for _, tc := range cases {
		network, address := splitProxyTarget(tc.input)
		if network != tc.network || address != tc.address {
			t.Errorf("splitProxyTarget(%q) = (%q, %q), want (%q, %q)",
				tc.input, network, address, tc.network, tc.address)
		}
	}
}

// TestRestartOnFailure runs a service that always exits 1 and verifies the
// supervisor restarts it at least three times before Stop is called.
func TestRestartOnFailure(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "count")
	script := fmt.Sprintf(
		`n=0; [ -f '%[1]s' ] && n=$(cat '%[1]s'); printf '%%d' $((n+1)) > '%[1]s'; exit 1`,
		countFile,
	)
	u := &unit.Unit{
		Name:       "failing.service",
		Kind:       unit.KindService,
		Type:       unit.TypeSimple,
		ExecStart:  []string{"/bin/sh", "-c", script},
		Restart:    unit.RestartOnFailure,
		RestartSec: 20 * time.Millisecond,
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer func() { close(dispStop); <-d.Done() }()
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{u}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(countFile)
		if err == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if n >= 3 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	sup.Stop()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not shut down")
	}

	data, _ := os.ReadFile(countFile)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if n < 3 {
		t.Errorf("service ran %d time(s), want at least 3", n)
	}
}
