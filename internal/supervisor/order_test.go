package supervisor

import (
	"strings"
	"testing"

	"github.com/emrul/container-init/unit"
)

func units(us ...*unit.Unit) []*unit.Unit { return us }

func u(name string, after ...string) *unit.Unit {
	return &unit.Unit{Name: name, After: after}
}

func before(name string, before ...string) *unit.Unit {
	return &unit.Unit{Name: name, Before: before}
}

func order(t *testing.T, us []*unit.Unit) []string {
	t.Helper()
	resolveBefore(us)
	ordered, err := topoSort(us)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	names := make([]string, 0, len(ordered))
	for _, x := range ordered {
		names = append(names, x.Name)
	}
	return names
}

func indexOf(t *testing.T, names []string, want string) int {
	t.Helper()
	for i, n := range names {
		if n == want {
			return i
		}
	}
	t.Fatalf("%q not in order %v", want, names)
	return -1
}

func TestBeforeOrdersTheStartList(t *testing.T) {
	// The regression this exists for: Before= parsed into the unit and was
	// read nowhere, so it ordered nothing while looking like it did.
	names := order(t, units(
		u("app.service"),
		before("accessibility.service", "app.service"),
	))
	if indexOf(t, names, "accessibility.service") > indexOf(t, names, "app.service") {
		t.Errorf("Before= did not order: got %v", names)
	}
}

func TestBeforeIsFoldedIntoTheTargetsAfter(t *testing.T) {
	// waitDeps -- not topoSort -- is what actually blocks a unit at run time,
	// and it reads After=. If the fold does not happen, ordering is correct in
	// the boot trace and still races in practice.
	app := u("app.service")
	us := units(app, before("accessibility.service", "app.service"))
	resolveBefore(us)
	found := false
	for _, dep := range app.After {
		if dep == "accessibility.service" {
			found = true
		}
	}
	if !found {
		t.Errorf("app.service.After = %v, want it to contain accessibility.service", app.After)
	}
}

func TestBeforeAndAfterAgreeingDoNotDuplicate(t *testing.T) {
	// A unit pair may legitimately declare both halves. Duplicated deps are
	// survivable but make indegree bookkeeping harder to reason about, so the
	// fold is idempotent.
	app := u("app.service", "accessibility.service")
	resolveBefore(units(app, before("accessibility.service", "app.service")))
	n := 0
	for _, dep := range app.After {
		if dep == "accessibility.service" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("dependency appears %d times in %v, want exactly 1", n, app.After)
	}
}

func TestBeforeNamingAnAbsentUnitIsIgnored(t *testing.T) {
	// Matches how topoSort and waitDeps treat an unknown After= name: an image
	// that does not ship the target unit must still boot.
	names := order(t, units(before("accessibility.service", "not-installed.service")))
	if len(names) != 1 || names[0] != "accessibility.service" {
		t.Errorf("got %v, want just accessibility.service", names)
	}
}

func TestBeforeSelfReferenceIsNotACycle(t *testing.T) {
	names := order(t, units(before("loop.service", "loop.service")))
	if len(names) != 1 {
		t.Errorf("got %v, want the unit to start normally", names)
	}
}

func TestBeforeParticipatesInCycleDetection(t *testing.T) {
	// a Before= b and b Before= a is a genuine cycle and must be reported,
	// not silently dropped or deadlocked on.
	us := units(before("a.service", "b.service"), before("b.service", "a.service"))
	resolveBefore(us)
	if _, err := topoSort(us); err == nil {
		t.Error("expected a cycle error")
	} else if !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Errorf("error = %q, want it to mention a cycle", err)
	}
}

func TestBeforeComposesWithAfter(t *testing.T) {
	// The real shape: accessibility must land after the window manager and
	// before the app that reads the setting at startup.
	names := order(t, units(
		u("window-manager.service"),
		u("custom-startup.service", "window-manager.service"),
		&unit.Unit{
			Name:   "accessibility.service",
			After:  []string{"window-manager.service"},
			Before: []string{"custom-startup.service"},
		},
	))
	wm := indexOf(t, names, "window-manager.service")
	a11y := indexOf(t, names, "accessibility.service")
	app := indexOf(t, names, "custom-startup.service")
	if !(wm < a11y && a11y < app) {
		t.Errorf("got %v, want window-manager < accessibility < custom-startup", names)
	}
}

func TestRequiresIsNotAnOrdering(t *testing.T) {
	// "a Requires= b" plus "a Before= b" is valid in systemd: b needs a
	// pulled in, and a must be up first. With Requires= counted as an
	// edge it was a cycle and the whole unit set was rejected.
	a := &unit.Unit{Name: "a.service", Requires: []string{"b.service"}, Before: []string{"b.service"}}
	names := order(t, units(a, u("b.service")))
	if indexOf(t, names, "a.service") > indexOf(t, names, "b.service") {
		t.Errorf("Before= did not order a first: got %v", names)
	}
}

func socketFor(name, service string, after ...string) *unit.Unit {
	return &unit.Unit{Name: name, Kind: unit.KindSocket, Service: service, After: after}
}

func TestSocketOrderedBeforeItsService(t *testing.T) {
	// No After= anywhere: the socket still starts (and so stops) on the
	// right side of the service it activates.
	names := order(t, units(u("a.service"), socketFor("z.socket", "a.service")))
	if indexOf(t, names, "z.socket") > indexOf(t, names, "a.service") {
		t.Errorf("got %v, want z.socket before a.service", names)
	}
}

func TestSocketAfterItsServiceIsACycle(t *testing.T) {
	// The implicit socket-before-service ordering meets an explicit
	// After= the other way: rejected before start, naming both, rather
	// than a shutdown where each waits for the other.
	us := units(u("a.service"), socketFor("a.socket", "a.service", "a.service"))
	_, err := topoSort(us)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	for _, want := range []string{"a.service", "a.socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
}
