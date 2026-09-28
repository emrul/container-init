package unit

import (
	"fmt"
	"testing"
)

func withConditionUser(t *testing.T, euid int, users map[string]uint32) {
	t.Helper()
	origEUID, origLookup := conditionEUID, lookupUID
	conditionEUID = func() int { return euid }
	lookupUID = func(name string) (uint32, error) {
		if uid, ok := users[name]; ok {
			return uid, nil
		}
		return 0, fmt.Errorf("user %q not found", name)
	}
	t.Cleanup(func() { conditionEUID, lookupUID = origEUID, origLookup })
}

func TestConditionUser(t *testing.T) {
	users := map[string]uint32{"1000": 1000, "app": 1000}
	cases := []struct {
		euid  int
		conds []string
		skip  bool
	}{
		{0, []string{"root"}, false},
		{1000, []string{"root"}, true},
		{0, []string{"!root"}, true},
		{1000, []string{"!root"}, false},
		{1000, []string{"1000"}, false},
		{1000, []string{"app"}, false},
		{0, []string{"app"}, true},
		{1000, []string{"!app"}, true},
		{1000, []string{"nobody-here"}, true},   // unresolvable never matches
		{1000, []string{"!nobody-here"}, false}, // ... so its negation holds
		{1000, []string{"!root", "app"}, false}, // all must hold
		{1000, []string{"!root", "root"}, true},
	}
	for _, tc := range cases {
		withConditionUser(t, tc.euid, users)
		u := &Unit{Name: "x.service", ConditionUser: tc.conds}
		evaluateConditions(u, OSLookup)
		if u.Condition.Skip != tc.skip {
			t.Errorf("euid=%d ConditionUser=%v: skip=%v (%s), want %v",
				tc.euid, tc.conds, u.Condition.Skip, u.Condition.Reason, tc.skip)
		}
	}
}

func TestConditionUserAtSystemWarns(t *testing.T) {
	dir := t.TempDir()
	writeUnit(t, dir, "x.service", `[Unit]
ConditionUser=@system

[Service]
ExecStart=/bin/true
`)
	units, warnings, err := LoadDir(dir, Options{})
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want one for @system", warnings)
	}
	if len(units[0].ConditionUser) != 0 {
		t.Errorf("ConditionUser = %v, want @system dropped", units[0].ConditionUser)
	}
}

func TestConditionEnvironment(t *testing.T) {
	env := map[string]string{"MODE": "root", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cases := []struct {
		conds []string
		skip  bool
	}{
		{[]string{"MODE"}, false},
		{[]string{"UNSET"}, true},
		{[]string{"EMPTY"}, false},
		{[]string{"MODE=root"}, false},
		{[]string{"MODE=user"}, true},
		{[]string{"!MODE"}, true},
		{[]string{"!UNSET"}, false},
		{[]string{"!EMPTY"}, true}, // set, though empty
		{[]string{"!MODE=root"}, true},
		{[]string{"!MODE=user"}, false},
		{[]string{"!UNSET=false"}, false}, // unset is not "false"
		{[]string{"MODE", "!MODE=user"}, false},
		{[]string{"MODE", "!MODE=root"}, true},
	}
	for _, tc := range cases {
		u := &Unit{Name: "x.service", ConditionEnvironment: tc.conds}
		evaluateConditions(u, lookup)
		if u.Condition.Skip != tc.skip {
			t.Errorf("ConditionEnvironment=%v: skip=%v (%s), want %v",
				tc.conds, u.Condition.Skip, u.Condition.Reason, tc.skip)
		}
	}
}
