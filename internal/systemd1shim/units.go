package systemd1shim

import (
	"sync"

	"github.com/godbus/dbus/v5"
)

// maxUnits bounds how many started units the shim remembers. Chrome
// starts a new app-<id>-<pid>.scope on every launch and the shim never
// learns when a scope ends, so the set drops its oldest entry once it
// is full.
const maxUnits = 1024

// unitSet records the units the shim has started, keyed by the object
// path GetUnit hands out for them, so the property stub can tell
// "a unit we started" from any other path under /unit.
type unitSet struct {
	mu    sync.Mutex
	max   int
	names map[dbus.ObjectPath]string
	order []dbus.ObjectPath // insertion order, oldest first
}

func newUnitSet(max int) *unitSet {
	return &unitSet{max: max, names: map[dbus.ObjectPath]string{}}
}

// add records name. Re-adding a known unit leaves its place in the
// eviction order unchanged.
func (s *unitSet) add(name string) {
	p := unitPath(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.names[p]; ok {
		return
	}
	if len(s.order) >= s.max {
		delete(s.names, s.order[0])
		s.order = s.order[1:]
	}
	s.names[p] = name
	s.order = append(s.order, p)
}

func (s *unitSet) remove(name string) {
	p := unitPath(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.names[p]; !ok {
		return
	}
	delete(s.names, p)
	for i, q := range s.order {
		if q == p {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// lookup returns the unit name recorded for path.
func (s *unitSet) lookup(p dbus.ObjectPath) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.names[p]
	return name, ok
}

// unitPath is the object path GetUnit returns for name.
func unitPath(name string) dbus.ObjectPath {
	return dbus.ObjectPath(string(managerPath) + "/unit/" + escapeUnitName(name))
}
