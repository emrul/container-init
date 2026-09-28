package unit

import (
	"testing"
	"time"
)

func TestStartLimitParsing(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		burst    int
		interval time.Duration
		warnings int
	}{
		{"neither: no limit", "[Service]\nExecStart=/bin/true\n", 0, 0, 0},
		{"both in [Unit]", "[Unit]\nStartLimitBurst=3\nStartLimitIntervalSec=30s\n[Service]\nExecStart=/bin/true\n", 3, 30 * time.Second, 0},
		{"both in [Service]", "[Service]\nExecStart=/bin/true\nStartLimitBurst=3\nStartLimitIntervalSec=30s\n", 3, 30 * time.Second, 0},
		{"burst only: default interval", "[Unit]\nStartLimitBurst=3\n[Service]\nExecStart=/bin/true\n", 3, 10 * time.Second, 0},
		{"interval only: default burst", "[Unit]\nStartLimitIntervalSec=1m\n[Service]\nExecStart=/bin/true\n", 5, time.Minute, 0},
		{"interval 0 disables", "[Unit]\nStartLimitBurst=3\nStartLimitIntervalSec=0\n[Service]\nExecStart=/bin/true\n", 3, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUnit(t, dir, "x.service", tc.body)
			units, warnings, err := LoadDir(dir, Options{})
			if err != nil {
				t.Fatalf("LoadDir: %v", err)
			}
			if len(warnings) != tc.warnings {
				t.Errorf("warnings = %v", warnings)
			}
			u := units[0]
			if u.StartLimitBurst != tc.burst || u.StartLimitIntervalSec != tc.interval {
				t.Errorf("start limit = %d / %v, want %d / %v",
					u.StartLimitBurst, u.StartLimitIntervalSec, tc.burst, tc.interval)
			}
		})
	}
}
