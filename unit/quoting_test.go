package unit

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQuotedEnvironmentAssignment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "quoted.service")
	if err := os.WriteFile(p, []byte("[Service]\nExecStart=/bin/true\nEnvironment=\"GREETING=hello world\" EMPTY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, warnings, err := LoadFile(p, Options{Strict: true})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("load: %v, warnings: %v", err, warnings)
	}
	if want := []string{"GREETING=hello world", "EMPTY="}; !reflect.DeepEqual(u.Environment, want) {
		t.Errorf("Environment = %#v, want %#v", u.Environment, want)
	}
}

func TestQuotedEnvironmentFilePreservesTrailingSpace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(p, []byte("VALUE='keep trailing space '\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvironmentFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"VALUE=keep trailing space "}; !reflect.DeepEqual(got, want) {
		t.Errorf("quoted value = %#v, want %#v", got, want)
	}
}
