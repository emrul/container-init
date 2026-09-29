package supervisor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/emrul/container-init/unit"
)

// resolveBefore rewrites every "u Before= X" into the equivalent
// "X After= u", in place, and must run before topoSort.
//
// Doing it as a rewrite rather than as extra edges inside topoSort is
// deliberate. topoSort fixes only the order of the start *list*; what actually
// blocks a unit at run time is waitDeps, which reads After=.
// Teaching topoSort about Before= on its own would produce ordering that looks
// right in the boot trace and still races in practice -- strictly worse than
// not supporting the directive, because it would look supported.
//
// Before this existed, Before= parsed into the unit struct and was read
// nowhere: accepted by --strict-units (it is a known directive, so no
// unknown-directive warning), silently no-ordering at run time.
//
// A Before= naming a unit that is not loaded is ignored, matching how
// topoSort and waitDeps treat an unknown After= name.
func resolveBefore(units []*unit.Unit) {
	byName := make(map[string]*unit.Unit, len(units))
	for _, u := range units {
		byName[u.Name] = u
	}
	for _, u := range units {
		for _, name := range u.Before {
			target, ok := byName[name]
			if !ok || target == u {
				continue
			}
			already := false
			for _, dep := range target.After {
				if dep == u.Name {
					already = true
					break
				}
			}
			if !already {
				target.After = append(target.After, u.Name)
			}
		}
	}
}

// orderedAfter returns, for each unit, the loaded units it is ordered
// after: its After= (with Before= already folded in by resolveBefore),
// plus the socket that activates it -- systemd's implicit ordering of a
// .socket before its service, which holds whether or not the service
// says After= itself. topoSort and shutdown both order by it, so a
// socket configured After= its own service is a cycle for both, caught
// before anything starts rather than stalling shutdown.
//
// waitDeps still reads After= alone: sockets are bound before any
// service starts, so the implicit edge has nothing to wait for then.
func orderedAfter(units []*unit.Unit) map[string][]string {
	loaded := make(map[string]bool, len(units))
	for _, u := range units {
		loaded[u.Name] = true
	}
	after := make(map[string][]string, len(units))
	add := func(u, dep string) {
		if loaded[dep] && !slices.Contains(after[u], dep) {
			after[u] = append(after[u], dep)
		}
	}
	for _, u := range units {
		for _, dep := range u.After {
			add(u.Name, dep)
		}
		if u.Kind == unit.KindSocket && loaded[u.Service] {
			add(u.Service, u.Name)
		}
	}
	return after
}

// topoSort returns units in start order: dependencies before dependents.
// Only ordering dependencies contribute edges. Cycles are reported.
//
// Requires= is a requirement, not an ordering: as in systemd, "A Requires=B"
// without "A After=B" starts both in parallel, and "A Requires=B" with
// "A Before=B" is valid and starts A first. Treating it as an edge made that
// second case a cycle.
//
// Before= does not contribute edges here: resolveBefore has already folded it
// into the target's After=. The implicit socket-before-service ordering does
// (see orderedAfter).
func topoSort(units []*unit.Unit) ([]*unit.Unit, error) {
	byName := make(map[string]*unit.Unit, len(units))
	for _, u := range units {
		byName[u.Name] = u
	}
	indeg := make(map[string]int, len(units))
	edges := make(map[string][]string, len(units))
	for _, u := range units {
		indeg[u.Name] += 0
	}
	for name, deps := range orderedAfter(units) {
		for _, dep := range deps {
			// "name After= dep" means dep must start before name.
			edges[dep] = append(edges[dep], name)
			indeg[name]++
		}
	}

	// Kahn's algorithm.
	var ready []string
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	var ordered []*unit.Unit
	for len(ready) > 0 {
		// Pop in stable filename order so multiple zero-indegree
		// roots boot deterministically.
		min := 0
		for i := 1; i < len(ready); i++ {
			if ready[i] < ready[min] {
				min = i
			}
		}
		name := ready[min]
		ready = append(ready[:min], ready[min+1:]...)
		ordered = append(ordered, byName[name])
		for _, next := range edges[name] {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
			}
		}
	}
	if len(ordered) != len(units) {
		var stuck []string
		for name, d := range indeg {
			if d > 0 {
				stuck = append(stuck, name)
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("dependency cycle in unit set, among %s "+
			"(a .socket is ordered before the service it activates)", strings.Join(stuck, ", "))
	}
	return ordered, nil
}
