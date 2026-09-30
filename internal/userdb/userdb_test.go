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
	// With audio as the primary group, video stays supplementary and
	// alice's passwd group (1500) is not listed.
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
	// Numeric uid, no /etc/passwd entry -- Resolve should still
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

func TestResolveEnvFileOverride(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	// A renamed user exists only in the generated files the
	// environment points at, as nss_wrapper would see them.
	dir := t.TempDir()
	pp := filepath.Join(dir, "passwd")
	gp := filepath.Join(dir, "group")
	if err := os.WriteFile(pp, []byte("renamed:x:1000:1000::/home/renamed:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gp, []byte("renamed:x:1000:\naudio:x:29:renamed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswdFileEnv, pp)
	t.Setenv(GroupFileEnv, gp)

	got, err := Resolve("renamed", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Identity{
		Username:            "renamed",
		UID:                 1000,
		GID:                 1000,
		Home:                "/home/renamed",
		SupplementaryGroups: []uint32{29},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	if _, err := Resolve("app-user", "", ""); err == nil {
		t.Error("app-user resolved from the default file despite the override")
	}
}

// An override file that does not exist yet falls back to the default,
// so a unit that runs before the file is generated still resolves; once
// the file exists it is read instead.
func TestResolveEnvFileMissingFallsBack(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	dir := t.TempDir()
	pp := filepath.Join(dir, "passwd")
	gp := filepath.Join(dir, "group")
	t.Setenv(PasswdFileEnv, pp)
	t.Setenv(GroupFileEnv, gp)

	got, err := Resolve("alice", "", "")
	if err != nil {
		t.Fatalf("Resolve before the override exists: %v", err)
	}
	if got.UID != 1500 || !reflect.DeepEqual(got.SupplementaryGroups, []uint32{29, 44}) {
		t.Errorf("got %#v, want alice from the default files", got)
	}

	if err := os.WriteFile(pp, []byte("renamed:x:1000:1000::/home/renamed:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gp, []byte("renamed:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("renamed", "", ""); err != nil {
		t.Errorf("Resolve after the override was written: %v", err)
	}
	if _, err := Resolve("alice", "", ""); err == nil {
		t.Error("alice resolved from the default file although the override now exists")
	}
}

func TestGID(t *testing.T) {
	withFakeFiles(t, samplePasswd, sampleGroup)
	if gid, err := GID("7777"); err != nil || gid != 7777 {
		t.Errorf("GID(7777) = %d, %v; want 7777 with or without a group entry", gid, err)
	}
	if _, err := GID("no-such-group"); err == nil {
		t.Error("GID of an unknown group name succeeded")
	}
	if gid, err := GID("audio"); err != nil || gid != 29 {
		t.Errorf("GID(audio) = %d, %v; want 29", gid, err)
	}
}
