package unit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validate runs the cross-directive constraint rules that can't be
// enforced at the applyDirective level.
func validate(u *Unit) error {
	switch u.Kind {
	case KindService:
		if len(u.ExecStart) == 0 {
			return fmt.Errorf("service has no ExecStart=")
		}
	case KindSocket:
		if len(u.ListenStream) == 0 && len(u.ListenDatagram) == 0 {
			return fmt.Errorf("socket has no ListenStream= or ListenDatagram=")
		}
		if u.Service == "" {
			// Default convention: foo.socket activates foo.service.
			base := strings.TrimSuffix(u.Name, ".socket")
			u.Service = base + ".service"
		}
		if u.ActivationMode == ActivationProxy && u.ProxyTarget == "" {
			return fmt.Errorf("ActivationMode=proxy requires ProxyTarget=")
		}
		if u.ActivationMode == ActivationNative && u.ProxyTarget != "" {
			return fmt.Errorf("ProxyTarget= is meaningful only with ActivationMode=proxy")
		}
	}
	return nil
}

// evaluateConditions resolves Condition* directives against the
// process environment / filesystem. A skipped unit is left in the unit
// list (so the supervisor can log it) but never started.
func evaluateConditions(u *Unit, lookup Lookup) {
	for _, p := range u.ConditionPathExists {
		if _, err := os.Stat(p); err != nil {
			u.Condition.Skip = true
			u.Condition.Reason = fmt.Sprintf("ConditionPathExists=%s unmet", p)
			return
		}
	}
	for _, pattern := range u.ConditionPathExistsGlob {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			u.Condition.Skip = true
			u.Condition.Reason = fmt.Sprintf("ConditionPathExistsGlob=%s unmet", pattern)
			return
		}
	}
	for _, e := range u.ConditionEnvironment {
		// "VAR=value" — present and equal required; "VAR" — present required.
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			if _, ok := lookup(e); !ok {
				u.Condition.Skip = true
				u.Condition.Reason = fmt.Sprintf("ConditionEnvironment=%s unset", e)
				return
			}
			continue
		}
		name, want := e[:eq], e[eq+1:]
		got, _ := lookup(name)
		if got != want {
			u.Condition.Skip = true
			u.Condition.Reason = fmt.Sprintf("ConditionEnvironment=%s (have %q, want %q)", e, got, want)
			return
		}
	}
}
