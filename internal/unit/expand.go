package unit

import (
	"strings"
)

// Expand resolves ${VAR} and ${VAR:-default} references in s against
// the provided lookup function. Bare $VAR and other shell forms are
// not recognised — only the brace forms are part of the directive-value
// expansion contract.
//
// Defaults may themselves contain ${...} references, e.g.
// ${APP_AUTH:-default:${APP_PW}} — nested references are resolved
// recursively. Brace depth is tracked when scanning for the outer
// '}' so the FIRST '}' inside the default doesn't prematurely close
// the outer reference.
//
// A literal "$$" yields a single "$" so unit values can carry the
// character verbatim.
//
// Lookup returns the variable's value and whether it was set. An unset
// variable in a ${VAR} (no default) form expands to the empty string,
// matching systemd's behaviour for absent EnvironmentFile entries.
func Expand(s string, lookup func(string) (string, bool)) string {
	if !strings.ContainsRune(s, '$') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// '$' at end of string — emit literally.
		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}
		next := s[i+1]
		if next == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if next != '{' {
			// Bare $VAR is not part of the supported subset; emit
			// the '$' verbatim and continue.
			b.WriteByte('$')
			i++
			continue
		}
		end, ok := findClose(s, i+2)
		if !ok {
			// Unterminated ${ — emit verbatim and stop trying to
			// parse further references.
			b.WriteString(s[i:])
			return b.String()
		}
		ref := s[i+2 : end]
		i = end + 1
		name, def, hasDef := splitDefault(ref)
		val, valOK := lookup(name)
		switch {
		case valOK && val != "":
			b.WriteString(val)
		case hasDef:
			// Recursively expand any ${...} inside the default.
			b.WriteString(Expand(def, lookup))
		default:
			// unset and no default → empty
		}
	}
	return b.String()
}

// findClose returns the index of the '}' that closes the '${' opened
// at position start-2. Nested '${' / '}' pairs are tracked so the
// scanner skips past inner refs (e.g. the '}' of ${APP_PW} inside
// ${APP_AUTH:-default:${APP_PW}}).
func findClose(s string, start int) (int, bool) {
	depth := 1
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			// Only treat '{' as opening a new ref when preceded by '$'.
			if i > 0 && s[i-1] == '$' {
				depth++
			}
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return -1, false
}

// splitDefault returns the name and (if present) default value from a
// reference body like "VAR" or "VAR:-default". The default may itself
// be empty ("VAR:-").
func splitDefault(ref string) (name, def string, hasDef bool) {
	if idx := strings.Index(ref, ":-"); idx >= 0 {
		return ref[:idx], ref[idx+2:], true
	}
	return ref, "", false
}
