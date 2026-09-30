package supervisor

import (
	"strings"
	"testing"

	"github.com/emrul/container-init/internal/pid1"
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
	// Before= orders the start list, not only the parsed unit.
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
	// pulled in, and a must be up first. Requires= adds no ordering
	// edge, so this is not a cycle.
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

func proxySocketFor(name, service string) *unit.Unit {
	u := socketFor(name, service)
	u.ActivationMode, u.ProxyTarget = unit.ActivationProxy, "127.0.0.1:9"
	return u
}

func TestCheck(t *testing.T) {
	// The --validate check agrees with New's: a sound set passes; an
	// explicit cycle, one through a socket's implicit ordering, and a
	// native socket sharing its service are all reported.
	cases := []struct {
		name  string
		units []*unit.Unit
		want  []string // substrings of the error; nil = accepted
	}{
		{"sound", units(u("a.service"), u("b.service", "a.service"), socketFor("a.socket", "a.service")), nil},
		{"Before= cycle", units(before("a.service", "b.service"), before("b.service", "a.service")), []string{"cycle"}},
		{"socket After= its service", units(u("a.service"), socketFor("a.socket", "a.service", "a.service")), []string{"cycle"}},
		{"two native sockets, one service",
			units(u("a.service"), socketFor("a.socket", "a.service"), socketFor("b.socket", "a.service")),
			[]string{"service a.service", "a.socket (native)", "b.socket (native)", "only socket for its service"}},
		{"native and proxy sockets, one service",
			units(u("a.service"), proxySocketFor("a.socket", "a.service"), socketFor("b.socket", "a.service")),
			[]string{"service a.service", "a.socket (proxy)", "b.socket (native)"}},
		{"two proxy sockets, one service",
			units(u("a.service"), proxySocketFor("a.socket", "a.service"), proxySocketFor("b.socket", "a.service")), nil},
		{"native sockets, a service each",
			units(u("a.service"), u("b.service"), socketFor("a.socket", "a.service"), socketFor("b.socket", "b.service")), nil},
		{"native sockets naming an unloaded service",
			units(socketFor("a.socket", "gone.service"), socketFor("b.socket", "gone.service")), nil},
		{"every shared service reported",
			units(u("a.service"), u("b.service"),
				socketFor("a1.socket", "a.service"), socketFor("a2.socket", "a.service"),
				socketFor("b1.socket", "b.service"), socketFor("b2.socket", "b.service")),
			[]string{"service a.service", "service b.service"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.units)
			if (err != nil) != (tc.want != nil) {
				t.Fatalf("Check = %v, want error %v", err, tc.want != nil)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if _, newErr := New(tc.units, nil, pid1.NewDispatcher(), nil); (newErr != nil) != (err != nil) {
				t.Errorf("Check = %v but New = %v; they must agree", err, newErr)
			}
		})
	}
}
