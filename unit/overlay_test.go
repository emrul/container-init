package unit

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLoadOverlayAdditive(t *testing.T) {
	core := t.TempDir()
	drop := t.TempDir()
	writeUnit(t, core, "core-a.service", "[Service]\nExecStart=/bin/true\n")
	writeUnit(t, drop, "image-b.service", "[Service]\nExecStart=/bin/true\n")

	units, warnings, overrides, err := LoadOverlay([]string{core, drop}, Options{})
	if err != nil {
		t.Fatalf("LoadOverlay: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(overrides) != 0 {
		t.Errorf("overrides: %v (additive case should have none)", overrides)
	}
	names := []string{}
	for _, u := range units {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	want := []string{"core-a.service", "image-b.service"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Errorf("unit[%d] = %v, want %v", i, names, want)
			break
		}
	}
}

func TestLoadOverlayOverrideByName(t *testing.T) {
	core := t.TempDir()
	drop := t.TempDir()
	writeUnit(t, core, "web.service", "[Service]\nExecStart=/bin/false\n")
	writeUnit(t, drop, "web.service", "[Service]\nExecStart=/usr/local/bin/myapp\n")

	units, _, overrides, err := LoadOverlay([]string{core, drop}, Options{})
	if err != nil {
		t.Fatalf("LoadOverlay: %v", err)
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides len = %d, want 1", len(overrides))
	}
	o := overrides[0]
	if o.Name != "web.service" {
		t.Errorf("Override.Name = %q", o.Name)
	}
	if filepath.Dir(o.BasePath) != core {
		t.Errorf("BasePath dir = %q, want %q", filepath.Dir(o.BasePath), core)
	}
	if filepath.Dir(o.OverridePath) != drop {
		t.Errorf("OverridePath dir = %q, want %q", filepath.Dir(o.OverridePath), drop)
	}
	// Effective unit must be the drop-in.
	if len(units) != 1 || units[0].ExecStart[0] != "/usr/local/bin/myapp" {
		t.Errorf("override didn't take: %#v", units)
	}
}

func TestLoadOverlayMissingDirSkipped(t *testing.T) {
	core := t.TempDir()
	writeUnit(t, core, "a.service", "[Service]\nExecStart=/bin/true\n")
	// drop dir intentionally absent — operators don't always ship one.
	units, warnings, overrides, err := LoadOverlay([]string{core, filepath.Join(core, "definitely-not-here")}, Options{})
	if err != nil {
		t.Fatalf("LoadOverlay: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(overrides) != 0 {
		t.Errorf("overrides: %v", overrides)
	}
	if len(units) != 1 {
		t.Errorf("units len = %d", len(units))
	}
}

func TestLoadOverlayPriorityIsListOrder(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	third := t.TempDir()
	writeUnit(t, first, "x.service", "[Service]\nExecStart=/bin/false\n")
	writeUnit(t, second, "x.service", "[Service]\nExecStart=/bin/middle\n")
	writeUnit(t, third, "x.service", "[Service]\nExecStart=/bin/last\n")

	units, _, overrides, err := LoadOverlay([]string{first, second, third}, Options{})
	if err != nil {
		t.Fatalf("LoadOverlay: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("units len = %d", len(units))
	}
	if units[0].ExecStart[0] != "/bin/last" {
		t.Errorf("last-wins broken: %v", units[0].ExecStart)
	}
	// Two override events: second-over-first and third-over-second.
	if len(overrides) != 2 {
		t.Errorf("overrides len = %d, want 2", len(overrides))
	}
}

func TestLoadOverlayWithRealMissingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := LoadOverlay([]string{dir}, Options{})
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
}
