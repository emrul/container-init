package supervisor

import (
	"testing"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/unit"
)

func newDepsSupervisor(t *testing.T, us ...*unit.Unit) *Supervisor {
	t.Helper()
	s, err := New(us, nil, pid1.NewDispatcher(), &cgroup.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestWaitDepsFailedRequirementFailsDependent(t *testing.T) {
	dep := &unit.Unit{Name: "dep.service"}
	u := &unit.Unit{Name: "u.service", After: []string{"dep.service"}, Requires: []string{"dep.service"}}
	s := newDepsSupervisor(t, dep, u)
	if !s.markFailed("dep.service") {
		t.Fatal("markFailed on an unsettled unit returned false")
	}
	failedDep, ok := s.waitDeps(u)
	if ok || failedDep != "dep.service" {
		t.Errorf("waitDeps = (%q, %v), want (dep.service, false)", failedDep, ok)
	}
}

func TestWaitDepsFailedAfterOnlyReleasesDependent(t *testing.T) {
	dep := &unit.Unit{Name: "dep.service"}
	u := &unit.Unit{Name: "u.service", After: []string{"dep.service"}}
	s := newDepsSupervisor(t, dep, u)
	s.markFailed("dep.service")
	if failedDep, ok := s.waitDeps(u); !ok {
		t.Errorf("waitDeps = (%q, false), want ok: After= alone must not propagate failure", failedDep)
	}
}

func TestMarkFailedAfterReadyIsNoOp(t *testing.T) {
	// A Type=simple that started (ready) and later died must not turn
	// into a failed requirement for units still waiting elsewhere.
	dep := &unit.Unit{Name: "dep.service"}
	u := &unit.Unit{Name: "u.service", After: []string{"dep.service"}, Requires: []string{"dep.service"}}
	s := newDepsSupervisor(t, dep, u)
	s.signalReady("dep.service")
	if s.markFailed("dep.service") {
		t.Error("markFailed on a ready unit returned true")
	}
	if failedDep, ok := s.waitDeps(u); !ok {
		t.Errorf("waitDeps = (%q, false), want ok", failedDep)
	}
}

func TestDependencyFailureCascades(t *testing.T) {
	a := &unit.Unit{Name: "a.service"}
	b := &unit.Unit{Name: "b.service", After: []string{"a.service"}, Requires: []string{"a.service"}}
	c := &unit.Unit{Name: "c.service", After: []string{"b.service"}, Requires: []string{"b.service"}}
	s := newDepsSupervisor(t, a, b, c)
	s.markFailed("a.service")
	failedDep, ok := s.waitDeps(b)
	if ok {
		t.Fatal("b should fail on a")
	}
	s.dependencyFailed(b, failedDep)
	if failedDep, ok := s.waitDeps(c); ok || failedDep != "b.service" {
		t.Errorf("waitDeps(c) = (%q, %v), want (b.service, false)", failedDep, ok)
	}
}

// Requires= without After= is a requirement, not an ordering: the
// dependent neither waits for the requirement nor is stopped by its
// failure, as in systemd.
func TestWaitDepsRequiresWithoutAfter(t *testing.T) {
	dep := &unit.Unit{Name: "dep.service"}
	u := &unit.Unit{Name: "u.service", Requires: []string{"dep.service"}}
	s := newDepsSupervisor(t, dep, u)

	// dep has not settled: waitDeps must not block on it.
	done := make(chan bool, 1)
	go func() {
		_, ok := s.waitDeps(u)
		done <- ok
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Error("waitDeps failed with dep unsettled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitDeps blocked on a Requires= dependency with no After=")
	}

	s.markFailed("dep.service")
	if failedDep, ok := s.waitDeps(u); !ok {
		t.Errorf("waitDeps = (%q, false), want ok: Requires= without After= must not propagate failure", failedDep)
	}
}

// A Requires= naming a unit that is not loaded fails the dependent at
// once, with or without After=; missing After= / Wants= names do not.
func TestWaitDepsMissingRequirement(t *testing.T) {
	cases := []struct {
		name   string
		u      *unit.Unit
		wantOK bool
	}{
		{"requires only", &unit.Unit{Name: "u.service", Requires: []string{"gone.service"}}, false},
		{"requires and after", &unit.Unit{Name: "u.service", Requires: []string{"gone.service"}, After: []string{"gone.service"}}, false},
		{"after only", &unit.Unit{Name: "u.service", After: []string{"gone.service"}}, true},
		{"wants only", &unit.Unit{Name: "u.service", Wants: []string{"gone.service"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDepsSupervisor(t, tc.u)
			failedDep, ok := s.waitDeps(tc.u)
			if ok != tc.wantOK {
				t.Fatalf("waitDeps = (%q, %v), want ok=%v", failedDep, ok, tc.wantOK)
			}
			if !ok && failedDep != "gone.service" {
				t.Errorf("failedDep = %q, want gone.service", failedDep)
			}
		})
	}
}

// A missing unit anywhere in the Requires= chain stops the unit, with
// no ordering involved, and Requires= cycles neither loop nor hide it.
func TestMissingRequirementAnywhereInChain(t *testing.T) {
	req := func(name string, reqs ...string) *unit.Unit {
		return &unit.Unit{Name: name, Requires: reqs}
	}
	cases := []struct {
		name  string
		units []*unit.Unit
		want  map[string]missingRequirement // units expected to fail
	}{
		{
			name:  "two levels",
			units: []*unit.Unit{req("a.service", "b.service"), req("b.service", "gone.service")},
			want: map[string]missingRequirement{
				"a.service": {"gone.service", "b.service"},
				"b.service": {"gone.service", "b.service"},
			},
		},
		{
			name: "three levels",
			units: []*unit.Unit{
				req("a.service", "b.service"), req("b.service", "c.service"), req("c.service", "gone.service"),
			},
			want: map[string]missingRequirement{
				"a.service": {"gone.service", "c.service"},
				"b.service": {"gone.service", "c.service"},
				"c.service": {"gone.service", "c.service"},
			},
		},
		{
			name: "cycle reaching a missing unit",
			units: []*unit.Unit{
				req("a.service", "b.service", "gone.service"), req("b.service", "a.service"),
			},
			want: map[string]missingRequirement{
				"a.service": {"gone.service", "a.service"},
				"b.service": {"gone.service", "a.service"},
			},
		},
		{
			name:  "cycle with nothing missing",
			units: []*unit.Unit{req("a.service", "b.service"), req("b.service", "a.service")},
			want:  map[string]missingRequirement{},
		},
		{
			name: "missing only behind Wants=",
			units: []*unit.Unit{
				req("a.service", "b.service"), {Name: "b.service", Wants: []string{"gone.service"}},
			},
			want: map[string]missingRequirement{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDepsSupervisor(t, tc.units...)
			for _, u := range tc.units {
				failedDep, ok := s.waitDeps(u)
				want, shouldFail := tc.want[u.Name]
				if ok == shouldFail {
					t.Errorf("%s: waitDeps = (%q, %v), want ok=%v", u.Name, failedDep, ok, !shouldFail)
					continue
				}
				if shouldFail {
					if got := s.missingReq[u.Name]; got != want {
						t.Errorf("%s: missing = %+v, want %+v", u.Name, got, want)
					}
				}
			}
		})
	}
}
