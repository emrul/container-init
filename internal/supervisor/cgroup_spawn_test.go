//go:build linux

package supervisor

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

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
	defer func() { close(dispStop); <-d.Done() }()
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
	// Through the Once: reading the field directly races the probe,
	// which runs in the spawning goroutine.
	if !sup.canSpawnIntoCgroup() {
		t.Error("probe did not enable spawning into the cgroup")
	}
}

// TestSeccompDeniedCloneIntoCgroupFallsBack: a seccomp profile may
// refuse clone3 with any errno it likes. With EPERM, units must still
// start, through the move-after-spawn fallback, rather than fail. A
// seccomp filter cannot be removed, so the scenario runs in a child
// copy of the test binary.
func TestSeccompDeniedCloneIntoCgroupFallsBack(t *testing.T) {
	const childEnv = "CONTAINER_INIT_TEST_SECCOMP_CHILD"
	if os.Getenv(childEnv) == "1" {
		seccompFallbackScenario(t)
		return
	}
	if !cgroup.New().Available() {
		t.Skip("cgroup-v2 not available")
	}
	if _, _, ok := seccompArch(); !ok {
		t.Skipf("no seccomp test filter for %s", runtime.GOARCH)
	}
	// glibc's pthread_create also uses clone3 and falls back to clone
	// only on ENOSYS, so a cgo-linked test binary (any -race build, or
	// a default build where cgo is available) aborts under an EPERM
	// filter. container-init itself is static and its threads use
	// clone; run this with CGO_ENABLED=0 to exercise it.
	if dynamicallyLinked(t) {
		t.Skip("needs a static test binary (CGO_ENABLED=0, no -race)")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSeccompDeniedCloneIntoCgroupFallsBack$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scenario failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cannot spawn into a cgroup") {
		t.Errorf("fallback was not logged:\n%s", out)
	}
}

func seccompFallbackScenario(t *testing.T) {
	denyClone3(t, syscall.EPERM)
	cg := cgroup.New()
	dir := t.TempDir()
	out := filepath.Join(dir, "cgroup")
	u := &unit.Unit{
		Name:      "seccomp-fallback.service",
		Kind:      unit.KindService,
		Type:      unit.TypeSimple,
		ExecStart: []string{"/bin/sh", "-c", `sleep 0.2; read x < /proc/self/cgroup; printf '%s' "$x" > ` + out + `; exec sleep 60`},
	}
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer func() { close(dispStop); <-d.Done() }()
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
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("unit did not start under a clone3-denying seccomp filter: %v", err)
	}
	leaf := strings.TrimPrefix(filepath.Join(cg.Base(), u.Name), "/sys/fs/cgroup")
	if s := strings.TrimPrefix(string(got), "0::"); s != leaf {
		t.Errorf("unit is in %q, want %q (placed after spawn)", s, leaf)
	}
	if sup.canSpawnIntoCgroup() {
		t.Error("probe enabled spawning into the cgroup despite clone3 being denied")
	}
}

// seccompArch returns the AUDIT_ARCH value and seccomp(2) syscall
// number for this architecture. clone3 is 435 on both.
func seccompArch() (arch, sysSeccomp uint32, ok bool) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xc000003e, 317, true
	case "arm64":
		return 0xc00000b7, 277, true
	}
	return 0, 0, false
}

// denyClone3 installs a seccomp filter on every thread of this process
// that fails clone3 with errno and allows everything else.
func denyClone3(t *testing.T, errno syscall.Errno) {
	t.Helper()
	arch, sysSeccomp, ok := seccompArch()
	if !ok {
		t.Fatalf("no seccomp filter for %s", runtime.GOARCH)
	}
	const (
		ldAbs     = 0x20 // BPF_LD | BPF_W | BPF_ABS
		jeqK      = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
		retK      = 0x06 // BPF_RET | BPF_K
		retAllow  = 0x7fff0000
		retErrno  = 0x00050000
		sysClone3 = 435
	)
	type sockFilter struct {
		code   uint16
		jt, jf uint8
		k      uint32
	}
	prog := []sockFilter{
		{ldAbs, 0, 0, 4}, // seccomp_data.arch
		{jeqK, 1, 0, arch},
		{retK, 0, 0, retAllow},
		{ldAbs, 0, 0, 0}, // seccomp_data.nr
		{jeqK, 0, 1, sysClone3},
		{retK, 0, 0, retErrno | uint32(errno)},
		{retK, 0, 0, retAllow},
	}
	fprog := struct {
		len    uint16
		filter *sockFilter
	}{uint16(len(prog)), &prog[0]}
	const prSetNoNewPrivs = 38
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		t.Fatalf("PR_SET_NO_NEW_PRIVS: %v", e)
	}
	const setModeFilter, filterFlagTsync = 1, 1
	if _, _, e := syscall.RawSyscall(uintptr(sysSeccomp), setModeFilter, filterFlagTsync, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		t.Fatalf("seccomp: %v", e)
	}
}

// dynamicallyLinked reports whether the running binary has an ELF
// interpreter, i.e. was linked against libc.
func dynamicallyLinked(t *testing.T) bool {
	t.Helper()
	f, err := elf.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}
