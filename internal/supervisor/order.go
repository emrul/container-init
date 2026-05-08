package supervisor

import (
	"fmt"

	"github.com/emrul/container-init/internal/unit"
)

// topoSort returns units in start order: dependencies before dependents.
// After= and Requires= both contribute edges. Cycles are reported.
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
	addEdge := func(from, to string) {
		// "to After= from" means from must start before to.
		if _, ok := byName[from]; !ok {
			return
		}
		if _, ok := byName[to]; !ok {
			return
		}
		edges[from] = append(edges[from], to)
		indeg[to]++
	}
	for _, u := range units {
		for _, dep := range u.After {
			addEdge(dep, u.Name)
		}
		for _, dep := range u.Requires {
			addEdge(dep, u.Name)
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
		return nil, fmt.Errorf("dependency cycle in unit set")
	}
	return ordered, nil
}
