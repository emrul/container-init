//go:build linux

package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

// cgroupOf returns the cgroup-v2 path of pid, as /proc/<pid>/cgroup
// shows it ("0::<path>").
func cgroupOf(t *testing.T, pid string) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		t.Fatalf("read cgroup of %s: %v", pid, err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "0::")
}

// TestSpawnStartsInsideUnitCgroup: the service is created inside its
// unit's cgroup, so what it sees from its first instruction and what it
// forks straight away are both in the leaf -- not in container-init's
// own cgroup, where cgroup.kill would never reach them.
func TestSpawnStartsInsideUnitCgroup(t *testing.T) {
	cg := cgroup.New()
	if !cg.Available() {
		t.Skipf("cgroup-v2 not available: %v", cg.Err())
	}
	dir := t.TempDir()
	firstLook := filepath.Join(dir, "first-look")
	childPID := filepath.Join(dir, "child-pid")
	// The builtin read runs before sh forks or execs anything; the
	// background sleep is forked as early as sh can manage.
	script := `read x < /proc/self/cgroup; printf '%s' "$x" > ` + firstLook +
		`; sleep 60 & printf '%s' $! > ` + childPID + `; exec sleep 60`
	u := &unit.Unit{
		Name:      "cgroup-spawn-probe.service",
		Kind:      unit.KindService,
		Type:      unit.TypeSimple,
		ExecStart: []string{"/bin/sh", "-c", script},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{u}, nil, d, cg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(childPID); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	leaf := strings.TrimPrefix(filepath.Join(cg.Base(), u.Name), "/sys/fs/cgroup")
	if got := cgroupOf(t, strconv.Itoa(os.Getpid())); got == leaf {
		t.Fatalf("test process is itself in the unit leaf %s", leaf)
	}
	first, err := os.ReadFile(firstLook)
	if err != nil {
		t.Fatalf("service never recorded its cgroup: %v", err)
	}
	if got := strings.TrimPrefix(string(first), "0::"); got != leaf {
		t.Errorf("service's first look at its cgroup = %q, want %q", got, leaf)
	}
	pid, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatalf("service never recorded its child: %v", err)
	}
	if got := cgroupOf(t, string(pid)); got != leaf {
		t.Errorf("early child is in %q, want %q", got, leaf)
	}
}

func TestCmdWithoutCgroupClearsTheRequest(t *testing.T) {
	attr := procAttr()
	spawnIntoCgroup(attr, 7)
	if !attr.UseCgroupFD || attr.CgroupFD != 7 {
		t.Fatalf("spawnIntoCgroup did not set the request: %+v", attr)
	}
	orig := exec.Command("/bin/true", "arg")
	orig.Dir = "/"
	orig.SysProcAttr = attr
	c := cmdWithoutCgroup(orig)
	if c.SysProcAttr.UseCgroupFD || c.SysProcAttr.CgroupFD != 0 {
		t.Errorf("copy still asks for a cgroup: %+v", c.SysProcAttr)
	}
	if !orig.SysProcAttr.UseCgroupFD {
		t.Error("cmdWithoutCgroup modified the original SysProcAttr")
	}
	if !c.SysProcAttr.Setpgid || c.Path != orig.Path || len(c.Args) != len(orig.Args) || c.Dir != orig.Dir {
		t.Errorf("copy lost fields: %+v", c)
	}
}

func TestCloneIntoCgroupUnsupported(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&os.PathError{Op: "fork/exec", Path: "/bin/x", Err: syscall.ENOSYS}, true},
		{&os.PathError{Op: "fork/exec", Path: "/bin/x", Err: syscall.EINVAL}, true},
		{&os.PathError{Op: "fork/exec", Path: "/bin/x", Err: syscall.ENOENT}, false},
		{&os.PathError{Op: "fork/exec", Path: "/bin/x", Err: syscall.EACCES}, false},
	} {
		if got := cloneIntoCgroupUnsupported(tc.err); got != tc.want {
			t.Errorf("cloneIntoCgroupUnsupported(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
