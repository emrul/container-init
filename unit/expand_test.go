package unit

import "testing"

func TestExpand(t *testing.T) {
	env := map[string]string{
		"APP_USER": "alice",
		"EMPTY":    "",
		"APP_PW":   "secret",
	}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	cases := []struct {
		in, want string
	}{
		// Bare strings are unchanged.
		{"plain", "plain"},
		// Set var.
		{"User=${APP_USER}", "User=alice"},
		// Set var with default -- set wins.
		{"User=${APP_USER:-app}", "User=alice"},
		// Unset var with default -- default wins.
		{"Group=${APP_GROUP:-app}", "Group=app"},
		// Unset var, no default -- empty.
		{"Home=${APP_HOME}", "Home="},
		// Empty-string env counts as unset for ${VAR:-default}.
		{"Val=${EMPTY:-fallback}", "Val=fallback"},
		// Empty default is allowed.
		{"Val=${APP_GROUP:-}", "Val="},
		// Bare $VAR is not recognised -- '$' passes through.
		{"X=$APP_USER", "X=$APP_USER"},
		// Literal $$ collapses to $.
		{"sum=$$1", "sum=$1"},
		// Two refs in the same value.
		{"a=${APP_USER}/${APP_HOME:-/home/app}", "a=alice//home/app"},
		// Unterminated ${ -- pass through verbatim.
		{"oops=${APP_USER", "oops=${APP_USER"},
		// Nested ${...} inside a default -- APP_PW set, expands.
		{"auth=${APP_AUTH:-default:${APP_PW}}", "auth=default:secret"},
		// Nested ${...:-...} inside a default with both unset -- empty fallback chain.
		{"auth=${APP_AUTH:-default:${MISSING:-fallback}}", "auth=default:fallback"},
		// Nested unset, no default → empty in the middle.
		{"auth=${APP_AUTH:-default:${MISSING}}", "auth=default:"},
		// Outer var set -- default (with nested ref) is ignored entirely.
		{"auth=${APP_USER:-other:${APP_PW}}", "auth=alice"},
		// Two nested refs in one default.
		{"x=${UNSET:-${APP_USER}/${APP_PW}}", "x=alice/secret"},
	}
	for _, c := range cases {
		got := Expand(c.in, lookup)
		if got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
