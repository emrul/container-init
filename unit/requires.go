package unit

// MissingRequirement names the first unit in a Requires= chain that is
// not loaded, and the unit whose Requires= names it.
type MissingRequirement struct {
	Name string // the unit that is not loaded
	Via  string // the unit whose Requires= names it
}

// MissingRequirements walks each unit's Requires= closure and records
// the first name in it that is not loaded. systemd builds the whole
// transaction before starting anything, so "A Requires=B" with
// "B Requires=gone" does not start A either, with or without ordering.
// Each unit gets its own breadth-first walk with a visited set, which
// handles Requires= cycles; memoising across units would not, since a
// result recorded part-way round a cycle can miss what the rest of the
// cycle requires.
func MissingRequirements(byName map[string]*Unit) map[string]MissingRequirement {
	out := make(map[string]MissingRequirement)
	for name := range byName {
		seen := map[string]bool{name: true}
		queue := []string{name}
	walk:
		for len(queue) > 0 {
			cur := byName[queue[0]]
			queue = queue[1:]
			for _, req := range cur.Requires {
				if seen[req] {
					continue
				}
				seen[req] = true
				if _, ok := byName[req]; !ok {
					out[name] = MissingRequirement{Name: req, Via: cur.Name}
					break walk
				}
				queue = append(queue, req)
			}
		}
	}
	return out
}
