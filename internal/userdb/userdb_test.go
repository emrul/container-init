package userdb

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// withFakeFiles redirects passwdPath / groupPath to fixture files for
// the duration of the test. Restores on cleanup.
func withFakeFiles(t *testing.T, passwd, group string) {
	t.Helper()
	dir := t.TempDir()
	pp := filepath.Join(dir, "passwd")
	gp := filepath.Join(dir, "group")
	if err := os.WriteFile(pp, []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gp, []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	origP, origG := passwdPathOverride, groupPathOverride
	passwdPathOverride = pp
	groupPathOverride = gp
	t.Cleanup(func() {
		passwdPathOverride = origP
		groupPathOverride = origG
	})
}

const samplePasswd = `root:x:0:0:root:/root:/bin/bash
app-user:x:1000:1000:app-user:/home/app-user:/bin/bash
alice:x:1500:1500:Alice user:/home/alice:/bin/bash
synth:x:2000:2000::/var/empty:/sbin/nologin
`

const sampleGroup = `root:x:0:
app-user:x:1000:
alice:x:1500:
audio:x:29:alice,app-user
video:x:44:alice
docker:x:998:bob
`

func TestResolveByName(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	got, err := Resolve("alice", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Identity{
		Username:            "alice",
		UID:                 1500,
		GID:                 1500,
		Home:                "/home/alice",
		SupplementaryGroups: []uint32{29, 44},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestResolveByNumericUID(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	got, err := Resolve("1500", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Username != "alice" || got.UID != 1500 || got.GID != 1500 {
		t.Errorf("got %#v", got)
	}
	if !reflect.DeepEqual(got.SupplementaryGroups, []uint32{29, 44}) {
		t.Errorf("supplementary = %v", got.SupplementaryGroups)
	}
}

func TestResolveExplicitGroupOverride(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	got, err := Resolve("alice", "audio", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.GID != 29 {
		t.Errorf("GID = %d, want 29 (audio)", got.GID)
	}
	// audio is now primary, video stays supplementary; alice (1500) drops
	// out because it's no longer the primary.
	want := []uint32{44}
	if !reflect.DeepEqual(got.SupplementaryGroups, want) {
		t.Errorf("supplementary = %v, want %v", got.SupplementaryGroups, want)
	}
}

func TestResolveHomeOverride(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	got, err := Resolve("alice", "", "/data/alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Home != "/data/alice" {
		t.Errorf("Home = %q, want /data/alice", got.Home)
	}
}

func TestResolveUnknownUser(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	if _, err := Resolve("nope", "", ""); err == nil {
		t.Fatalf("expected error for unknown user")
	}
}

func TestResolveNumericNoPasswdEntry(t *testing.T) {
	// Numeric uid, no /etc/passwd entry — Resolve should still
	// succeed with synthetic name; supplementary groups empty.
	withFakeFiles(t, samplePasswd, sampleGroup)
	got, err := Resolve("9999", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UID != 9999 {
		t.Errorf("UID = %d, want 9999", got.UID)
	}
	if len(got.SupplementaryGroups) != 0 {
		t.Errorf("supplementary = %v, want empty", got.SupplementaryGroups)
	}
}

func TestResolveEmptyUserError(t *testing.T) {
	if _, err := Resolve("", "", ""); err == nil {
		t.Fatal("expected error for empty user")
	}
}

func TestSupplementaryGroupsExcludesPrimary(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	// app-user's primary GID is 1000 and 'audio' lists app-user.
	got, err := Resolve("app-user", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(got.SupplementaryGroups, []uint32{29}) {
		t.Errorf("supplementary = %v, want [29]", got.SupplementaryGroups)
	}
}
