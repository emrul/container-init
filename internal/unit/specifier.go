package unit

import (
	"os"
	"strings"
)

// SpecifierLookup resolves a single specifier letter (the byte after %)
// to its replacement string. Returning ok=false leaves the specifier
// verbatim in the output, matching how systemd treats unsupported
// specifiers in a fixed subset.
type SpecifierLookup func(byte) (string, bool)

// UnitSpecifiers builds the lookup function for unit fileName. The
// supported subset is %n (full unit name), %N (unit name without
// extension), %H (hostname). Everything else returns ok=false and
// passes through verbatim.
func UnitSpecifiers(fileName string) SpecifierLookup {
	return func(c byte) (string, bool) {
		switch c {
		case 'n':
			return fileName, true
		case 'N':
			if idx := strings.LastIndex(fileName, "."); idx > 0 {
				return fileName[:idx], true
			}
			return fileName, true
		case 'H':
			h, err := os.Hostname()
			if err != nil {
				return "", false
			}
			return h, true
		}
		return "", false
	}
}

// ExpandSpecifiers replaces %n / %N / %H in s using lookup. A literal
// "%%" yields a single "%". Unknown specifiers (lookup returns
// ok=false) pass through verbatim — the parse path elsewhere can decide
// whether to warn about them; we don't drop the bytes silently.
func ExpandSpecifiers(s string, lookup SpecifierLookup) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('%')
			continue
		}
		next := s[i+1]
		if next == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		if val, ok := lookup(next); ok {
			b.WriteString(val)
			i++
			continue
		}
		b.WriteByte('%')
	}
	return b.String()
}
