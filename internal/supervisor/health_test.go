//go:build linux

package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/health"
	"github.com/emrul/container-init/internal/statefile"
	"github.com/emrul/container-init/unit"
)

// healthOf runs `container-init health` over path.
func healthOf(path string, args ...string) (int, string) {
	var out bytes.Buffer
	code := health.Main(append([]string{"--state-file", path}, args...), &out, time.Now())
	return code, out.String()
}

// A required setup oneshot is unhealthy while it runs and healthy once
// it has succeeded, with or without RemainAfterExit=yes.
func TestHealthOneshotEndToEnd(t *testing.T) {
	for _, remain := range []bool{false, true} {
		t.Run(fmt.Sprintf("RemainAfterExit=%v", remain), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			gate := filepath.Join(t.TempDir(), "go")
			setup := shService("setup.service", unit.TypeOneshot, fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.02; done", gate))
			setup.RemainAfterExit = remain
			sup := stateSupervisor(t, path, nil, setup)
			run(t, sup)
			waitStateFile(t, path, "showed setup running", func(f *statefile.File) bool {
				return f.Units[setup.Name].Runs == 1
			})
			if code, out := healthOf(path, "--require", setup.Name); code != 1 {
				t.Errorf("during the run: health = %d %q, want 1", code, out)
			}
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waitStateFile(t, path, "showed setup done", func(f *statefile.File) bool {
				return f.Units[setup.Name].Active != "activating"
			})
			if code, out := healthOf(path, "--require", setup.Name); code != 0 {
				t.Errorf("after success: health = %d %q, want 0", code, out)
			}
		})
	}
}

// A heartbeat that stops advancing makes the container unhealthy once
// `written` is older than --max-age.
func TestHealthStaleHeartbeatEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sup := stateSupervisor(t, path, func(w *stateWriter) {
		w.tick, w.budget = 50*time.Millisecond, 50*time.Millisecond
	}, shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
	sup.live = newStuckReaper(false)
	run(t, sup)
	waitStateFile(t, path, "showed the daemon", func(f *statefile.File) bool {
		return f.Units["daemon.service"].Active == "active"
	})
	if code, out := healthOf(path, "--require", "daemon.service"); code != 0 {
		t.Errorf("fresh file: health = %d %q, want 0", code, out)
	}
	time.Sleep(2100 * time.Millisecond)
	if code, out := healthOf(path, "--require", "daemon.service", "--max-age", "1s"); code != 1 {
		t.Errorf("stale heartbeat: health = %d %q, want 1", code, out)
	}
}

// During and after reverse shutdown the container is unhealthy.
func TestHealthStoppingEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sup := stateSupervisor(t, path, nil, shService("daemon.service", unit.TypeSimple, "exec sleep 60"))
	stop := run(t, sup)
	waitStateFile(t, path, "showed the daemon", func(f *statefile.File) bool {
		return f.Units["daemon.service"].Active == "active"
	})
	stop()
	if code, out := healthOf(path); code != 1 {
		t.Errorf("after shutdown: health = %d %q, want 1", code, out)
	}
}
