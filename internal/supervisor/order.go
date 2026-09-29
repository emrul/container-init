package supervisor

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/emrul/container-init/unit"
)

// orderUnits returns units in start order, or the reason they cannot be
// ordered: the ordering part of checkUnits, which New makes before
// anything starts and Check makes for --validate.
//
// Before= is folded into the target's After= first, in place, so that
// both topoSort and waitDeps honour it without either needing to know it
// exists.
func orderUnits(units []*unit.Unit) ([]*unit.Unit, error) {
	resolveBefore(units)
	return topoSort(units)
}

// Check reports whether New would accept units: they can be ordered --
// no dependency cycle, counting a socket's implicit ordering before its
// service -- and no native-mode socket shares its service (see
// checkSharedSockets). It is the same check New makes, so --validate
// catches at build time what would otherwise fail at startup. Like New,
// it folds Before= into After= in place.
func Check(units []*unit.Unit) error {
	_, err := checkUnits(units)
	return err
}

// checkUnits is New's check of the whole unit set: the units in start
// order, or why they cannot be started.
func checkUnits(units []*unit.Unit) ([]*unit.Unit, error) {
	ordered, err := orderUnits(units)
	if err != nil {
		return nil, err
	}
	if err := checkSharedSockets(units); err != nil {
		return nil, err
	}
	return ordered, nil
}

// checkSharedSockets rejects a service that a native-mode socket
// activates alongside any other socket. A native activation passes the
// service only the listener that activated it, and runs under the
// service's run lock; while it runs, another socket's activation waits
// for the service to exit, so that socket's clients would queue,
// unserved, for as long as the service lives -- possibly the whole
// container. (systemd instead passes every socket of the service at
// start.) Several proxy-mode sockets may share a service: a proxied
// connection is served by whichever helper is running, without waiting
// for the lock. A socket naming a service that is not loaded never
// activates anything, so is not counted.
func checkSharedSockets(units []*unit.Unit) error {
	loaded := make(map[string]bool, len(units))
	for _, u := range units {
		loaded[u.Name] = true
	}
	var services []string
	sockets := map[string][]*unit.Unit{}
	for _, u := range units {
		if u.Kind != unit.KindSocket || !loaded[u.Service] {
			continue
		}
		if sockets[u.Service] == nil {
			services = append(services, u.Service)
		}
		sockets[u.Service] = append(sockets[u.Service], u)
	}
	var errs []error
	for _, svc := range services {
		socks := sockets[svc]
		if len(socks) < 2 || !slices.ContainsFunc(socks, isNative) {
			continue
		}
		desc := make([]string, len(socks))
		for i, s := range socks {
			desc[i] = fmt.Sprintf("%s (%s)", s.Name, s.ActivationMode)
		}
		errs = append(errs, fmt.Errorf("service %s is activated by %d sockets, %s: "+
			"a native-mode socket must be the only socket for its service, "+
			"since its service is passed only that socket's listener and the "+
			"others' clients would wait until it exits; give each native socket "+
			"its own service, or make every socket for %s ActivationMode=proxy",
			svc, len(socks), strings.Join(desc, ", "), svc))
	}
	return errors.Join(errs...)
}

func isNative(u *unit.Unit) bool { return u.ActivationMode == unit.ActivationNative }

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
		hint := ""
		for name, d := range indeg {
			if d > 0 {
				stuck = append(stuck, name)
				if byName[name].Kind == unit.KindSocket {
					hint = " (a .socket is ordered before the service it activates)"
				}
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("dependency cycle in unit set, among %s%s", strings.Join(stuck, ", "), hint)
	}
	return ordered, nil
}
