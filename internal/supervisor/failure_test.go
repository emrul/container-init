//go:build linux

package supervisor

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

// TestStartFailureSettlesDependents: a unit whose ExecStart cannot be
// exec'd must release After=-only dependents and fail Requires=
// dependents, instead of leaving both waiting forever.
func TestStartFailureSettlesDependents(t *testing.T) {
	dir := t.TempDir()
	orderedMark := filepath.Join(dir, "ordered")
	requiringMark := filepath.Join(dir, "requiring")

	broken := &unit.Unit{
		Name:      "broken.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{filepath.Join(dir, "does-not-exist")},
		Restart:   unit.RestartNo,
	}
	ordered := &unit.Unit{
		Name:      "ordered.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"broken.service"},
		ExecStart: []string{"/bin/touch", orderedMark},
	}
	requiring := &unit.Unit{
		Name:      "requiring.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"broken.service"},
		Requires:  []string{"broken.service"},
		ExecStart: []string{"/bin/touch", requiringMark},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{broken, ordered, requiring}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	for _, name := range []string{"ordered.service", "requiring.service"} {
		select {
		case <-sup.ready[name]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
	// ordered.service is a oneshot: ready means it ran.
	if _, err := os.Stat(orderedMark); err != nil {
		t.Errorf("After=-only dependent did not run: %v", err)
	}
	if _, err := os.Stat(requiringMark); err == nil {
		t.Error("Requires= dependent ran despite its requirement failing")
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !sup.failed["broken.service"] || !sup.failed["requiring.service"] {
		t.Errorf("failed = %v, want broken.service and requiring.service", sup.failed)
	}
}

// TestNonRootUserUnits runs only without root (CI's runner user, or a
// container started with --user): a User= unit resolving to our own
// identity runs as-is, and one wanting another uid fails to start.
func TestNonRootUserUnits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root euid")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "ran")
	uid, gid := strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid())
	same := &unit.Unit{
		Name:      "same.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		User:      uid,
		Group:     gid,
		ExecStart: []string{"/bin/touch", mark},
	}
	other := &unit.Unit{
		Name:      "other.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		User:      strconv.Itoa(os.Geteuid() + 1),
		Group:     gid,
		ExecStart: []string{"/bin/true"},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{same, other}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	for _, name := range []string{"same.service", "other.service"} {
		select {
		case <-sup.ready[name]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("same-identity User= unit did not run: %v", err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.failed["same.service"] || !sup.failed["other.service"] {
		t.Errorf("failed = %v, want only other.service", sup.failed)
	}
}

// TestSkippedServiceSocketNotBound: a socket must not listen for a
// service whose conditions skipped it, in either activation mode --
// otherwise the first client would start it anyway.
func TestSkippedServiceSocketNotBound(t *testing.T) {
	for _, mode := range []unit.ActivationMode{unit.ActivationNative, unit.ActivationProxy} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			sockPath := filepath.Join(dir, "sock")
			mark := filepath.Join(dir, "ran")
			sock := &unit.Unit{
				Name:           "gated.socket",
				Kind:           unit.KindSocket,
				ListenStream:   []unit.Listener{{Network: "unix", Address: sockPath}},
				ActivationMode: mode,
				Service:        "gated.service",
			}
			if mode == unit.ActivationProxy {
				sock.ProxyTarget = filepath.Join(dir, "private")
			}
			svc := &unit.Unit{
				Name:      "gated.service",
				Kind:      unit.KindService,
				Type:      unit.TypeSimple,
				ExecStart: []string{"/bin/touch", mark},
				Condition: unit.Condition{Skip: true, Reason: "ConditionUser=root unmet"},
			}

			d := pid1.NewDispatcher()
			dispStop := make(chan struct{})
			defer close(dispStop)
			d.Start(dispStop)

			sup, err := New([]*unit.Unit{sock, svc}, nil, d, &cgroup.Manager{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			runDone := make(chan struct{})
			go func() { sup.Run(); close(runDone) }()
			defer func() {
				sup.Stop()
				<-runDone
			}()

			select {
			case <-sup.ready["gated.service"]:
			case <-time.After(5 * time.Second):
				t.Fatal("gated.service never settled")
			}
			if c, err := net.Dial("unix", sockPath); err == nil {
				c.Close()
				t.Error("socket of a skipped service is listening")
			}
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(mark); err == nil {
				t.Error("skipped service ran")
			}
		})
	}
}

// TestStartLimitFailsUnit: a oneshot that keeps failing under
// Restart=on-failure runs StartLimitBurst times, then fails for good
// and fails the unit that Requires= it.
func TestStartLimitFailsUnit(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	script := fmt.Sprintf(
		`n=0; [ -f '%[1]s' ] && n=$(cat '%[1]s'); printf '%%d' $((n+1)) > '%[1]s'; exit 1`,
		countFile,
	)
	flaky := &unit.Unit{
		Name:                  "flaky.service",
		Kind:                  unit.KindService,
		Type:                  unit.TypeOneshot,
		ExecStart:             []string{"/bin/sh", "-c", script},
		Restart:               unit.RestartOnFailure,
		RestartSec:            10 * time.Millisecond,
		StartLimitBurst:       3,
		StartLimitIntervalSec: time.Minute,
	}
	dependent := &unit.Unit{
		Name:      "dependent.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"flaky.service"},
		Requires:  []string{"flaky.service"},
		ExecStart: []string{"/bin/true"},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	sup, err := New([]*unit.Unit{flaky, dependent}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	select {
	case <-sup.ready["dependent.service"]:
	case <-time.After(5 * time.Second):
		t.Fatal("dependent.service never settled")
	}
	time.Sleep(100 * time.Millisecond) // a 4th run would land well within this
	data, _ := os.ReadFile(countFile)
	if n, _ := strconv.Atoi(strings.TrimSpace(string(data))); n != 3 {
		t.Errorf("flaky.service ran %d time(s), want 3", n)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !sup.failed["flaky.service"] || !sup.failed["dependent.service"] {
		t.Errorf("failed = %v, want flaky.service and dependent.service", sup.failed)
	}
}

// countingScript returns a shell command that increments the counter in
// f, then runs tail.
func countingScript(f, tail string) string {
	return fmt.Sprintf(`n=0; [ -f '%[1]s' ] && n=$(cat '%[1]s'); printf '%%d' $((n+1)) > '%[1]s'; %[2]s`, f, tail)
}

func readCount(f string) int {
	data, _ := os.ReadFile(f)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

// runUnits starts a supervisor over us and stops it at test end.
func runUnits(t *testing.T, us ...*unit.Unit) *Supervisor {
	t.Helper()
	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	d.Start(dispStop)
	sup, err := New(us, nil, d, &cgroup.Manager{})
	if err != nil {
		close(dispStop)
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	t.Cleanup(func() {
		sup.Stop()
		<-runDone
		close(dispStop)
	})
	return sup
}

// waitCount waits until f counts at least n, then a little longer so
// anything that should not happen has time to.
func waitCount(t *testing.T, f string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for readCount(f) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
}

// TestOnFailureFiresOnceWhenUnitGivesUp: OnFailure= fires when the
// unit is failed for good, not on each failure it restarts from.
func TestOnFailureFiresOnceWhenUnitGivesUp(t *testing.T) {
	cases := []struct {
		name    string
		restart unit.RestartPolicy
		burst   int
		runs    int
	}{
		{"Restart=no", unit.RestartNo, 0, 1},
		{"Restart=on-failure until start limit", unit.RestartOnFailure, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			failCount := filepath.Join(dir, "fail")
			handlerCount := filepath.Join(dir, "handler")
			failing := &unit.Unit{
				Name:                  "failing.service",
				Kind:                  unit.KindService,
				Type:                  unit.TypeSimple,
				ExecStart:             []string{"/bin/sh", "-c", countingScript(failCount, "exit 1")},
				Restart:               tc.restart,
				RestartSec:            10 * time.Millisecond,
				StartLimitBurst:       tc.burst,
				StartLimitIntervalSec: time.Minute,
				OnFailure:             []string{"handler.service"},
			}
			handler := &unit.Unit{
				Name:      "handler.service",
				Kind:      unit.KindService,
				Type:      unit.TypeOneshot,
				ExecStart: []string{"/bin/sh", "-c", countingScript(handlerCount, "exit 0")},
			}
			runUnits(t, failing, handler)
			waitCount(t, failCount, tc.runs)
			if n := readCount(failCount); n != tc.runs {
				t.Errorf("failing.service ran %d time(s), want %d", n, tc.runs)
			}
			if n := readCount(handlerCount); n != 1 {
				t.Errorf("handler.service ran %d time(s), want 1", n)
			}
		})
	}
}

// TestOnFailureNotFiredWhileRestarting: a unit that fails and is
// restarted into success never reaches the failed state.
func TestOnFailureNotFiredWhileRestarting(t *testing.T) {
	dir := t.TempDir()
	runCount := filepath.Join(dir, "runs")
	handlerCount := filepath.Join(dir, "handler")
	flaky := &unit.Unit{
		Name:       "flaky.service",
		Kind:       unit.KindService,
		Type:       unit.TypeOneshot,
		ExecStart:  []string{"/bin/sh", "-c", countingScript(runCount, `[ "$n" -ge 1 ]`)}, // fails once
		Restart:    unit.RestartOnFailure,
		RestartSec: 10 * time.Millisecond,
		OnFailure:  []string{"handler.service"},
	}
	handler := &unit.Unit{
		Name:      "handler.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		ExecStart: []string{"/bin/sh", "-c", countingScript(handlerCount, "exit 0")},
	}
	sup := runUnits(t, flaky, handler)
	select {
	case <-sup.ready["flaky.service"]:
	case <-time.After(5 * time.Second):
		t.Fatal("flaky.service never became ready")
	}
	time.Sleep(200 * time.Millisecond)
	if n := readCount(runCount); n != 2 {
		t.Errorf("flaky.service ran %d time(s), want 2", n)
	}
	if n := readCount(handlerCount); n != 0 {
		t.Errorf("handler.service ran %d time(s), want 0", n)
	}
}

// TestStartLimitCoversOnFailure: an OnFailure= target is started
// through the same start limit as any other unit, so two failing units
// sharing one handler with StartLimitBurst=1 run it once.
func TestStartLimitCoversOnFailure(t *testing.T) {
	dir := t.TempDir()
	handlerCount := filepath.Join(dir, "handler")
	failing := func(name string) *unit.Unit {
		return &unit.Unit{
			Name:      name,
			Kind:      unit.KindService,
			Type:      unit.TypeSimple,
			ExecStart: []string{"/bin/sh", "-c", "exit 1"},
			OnFailure: []string{"handler.service"},
		}
	}
	handler := &unit.Unit{
		Name:                  "handler.service",
		Kind:                  unit.KindService,
		Type:                  unit.TypeOneshot,
		ExecStart:             []string{"/bin/sh", "-c", countingScript(handlerCount, "exit 0")},
		StartLimitBurst:       1,
		StartLimitIntervalSec: time.Minute,
	}
	runUnits(t, failing("a.service"), failing("b.service"), handler)
	waitCount(t, handlerCount, 1)
	if n := readCount(handlerCount); n != 1 {
		t.Errorf("handler.service ran %d time(s), want 1 (StartLimitBurst=1)", n)
	}
}

// syncBuffer is a bytes.Buffer safe to write from the log package and
// read from the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestOnFailureCycleWithSpentLimits: a.service and b.service name each
// other in OnFailure=, and both start limits are spent. A refused
// invocation must not chain, or the pair re-fires each other forever.
func TestOnFailureCycleWithSpentLimits(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	failing := func(name, onFailure string, burst int) *unit.Unit {
		return &unit.Unit{
			Name:                  name,
			Kind:                  unit.KindService,
			Type:                  unit.TypeOneshot,
			ExecStart:             []string{"/bin/sh", "-c", "exit 1"},
			OnFailure:             []string{onFailure},
			StartLimitBurst:       burst,
			StartLimitIntervalSec: time.Minute,
		}
	}
	// a (Type=simple, so it runs at boot although it is an OnFailure=
	// target) spends its budget and fires b, which spends its own. c
	// fires b too; one of the two invocations is refused, and a
	// chaining refusal would fire a, refused in turn, and so on.
	a := failing("a.service", "b.service", 1)
	a.Type = unit.TypeSimple
	b := failing("b.service", "a.service", 1)
	c := failing("c.service", "b.service", 0)
	c.Type = unit.TypeSimple
	runUnits(t, a, b, c)

	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(logs.String(), "start limit hit"); n != 1 {
		t.Errorf("%d refused OnFailure= invocation(s), want 1; log:\n%s", n, logs.String())
	}
}

// TestMissingRequirementNotStarted: a unit whose Requires= names a unit
// that does not exist is not started, and fails the units that require
// it in turn; a unit that only orders After= the missing name starts.
func TestMissingRequirementNotStarted(t *testing.T) {
	dir := t.TempDir()
	orphanMark := filepath.Join(dir, "orphan")
	chainedMark := filepath.Join(dir, "chained")
	orderedMark := filepath.Join(dir, "ordered")
	indirectMark := filepath.Join(dir, "indirect")

	orphan := &unit.Unit{
		Name:      "orphan.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		Requires:  []string{"not-installed.service"},
		ExecStart: []string{"/bin/touch", orphanMark},
	}
	chained := &unit.Unit{
		Name:      "chained.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"orphan.service"},
		Requires:  []string{"orphan.service"},
		ExecStart: []string{"/bin/touch", chainedMark},
	}
	ordered := &unit.Unit{
		Name:      "ordered.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		After:     []string{"not-installed.service"},
		ExecStart: []string{"/bin/touch", orderedMark},
	}

	d := pid1.NewDispatcher()
	dispStop := make(chan struct{})
	defer close(dispStop)
	d.Start(dispStop)

	// Requires= the orphan with no ordering: the missing unit two levels
	// down still keeps it from starting.
	indirect := &unit.Unit{
		Name:      "indirect.service",
		Kind:      unit.KindService,
		Type:      unit.TypeOneshot,
		Requires:  []string{"orphan.service"},
		ExecStart: []string{"/bin/touch", indirectMark},
	}

	sup, err := New([]*unit.Unit{orphan, chained, ordered, indirect}, nil, d, &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan struct{})
	go func() { sup.Run(); close(runDone) }()
	defer func() {
		sup.Stop()
		<-runDone
	}()

	for _, name := range []string{"orphan.service", "chained.service", "ordered.service", "indirect.service"} {
		select {
		case <-sup.ready[name]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never settled", name)
		}
	}
	if _, err := os.Stat(orphanMark); err == nil {
		t.Error("unit ran despite its Requires= naming a unit that does not exist")
	}
	if _, err := os.Stat(chainedMark); err == nil {
		t.Error("dependent of a not-started unit ran")
	}
	if _, err := os.Stat(orderedMark); err != nil {
		t.Errorf("After= a missing unit blocked the start: %v", err)
	}
	if _, err := os.Stat(indirectMark); err == nil {
		t.Error("unit ran although its Requires= chain reaches a unit that does not exist")
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !sup.failed["orphan.service"] || !sup.failed["chained.service"] || !sup.failed["indirect.service"] || sup.failed["ordered.service"] {
		t.Errorf("failed = %v, want orphan, chained and indirect only", sup.failed)
	}
}
