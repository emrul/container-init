package unit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseEnvironmentFileTable exercises the documented systemd
// quoting / continuation / comment rules through the line-shape parser
// surface. Cases are driven through parseEnvFileReader so the test
// doesn't need disk I/O.
func TestParseEnvironmentFileTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain",
			in:   "FOO=bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "leading_whitespace_in_value_stripped",
			in:   "FOO=   bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "trailing_whitespace_in_unquoted_value_stripped",
			in:   "FOO=bar   \n",
			want: []string{"FOO=bar"},
		},
		{
			name: "key_trailing_whitespace_stripped",
			in:   "FOO   =bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "single_quoted_preserves_spaces_and_escapes",
			in:   `FOO='line with spaces and \n literal'` + "\n",
			want: []string{`FOO=line with spaces and \n literal`},
		},
		{
			name: "double_quoted_processes_escapes",
			in:   `FOO="tab\there\nnewline"` + "\n",
			want: []string{"FOO=tab\there\nnewline"},
		},
		{
			name: "double_quoted_unknown_escape_preserved",
			in:   `FOO="bell:\x07"` + "\n",
			want: []string{`FOO=bell:\x07`},
		},
		{
			name: "double_quoted_escaped_quote",
			in:   `FOO="he said \"hi\""` + "\n",
			want: []string{`FOO=he said "hi"`},
		},
		{
			name: "double_quoted_dollar_literal",
			in:   `FOO="$5\$"` + "\n",
			want: []string{`FOO=$5$`},
		},
		{
			name: "concat_segments",
			in:   `FOO=foo'bar'"baz"` + "\n",
			want: []string{"FOO=foobarbaz"},
		},
		{
			name: "comment_hash",
			in:   "# this is a comment\nFOO=bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "comment_semicolon",
			in:   "; another comment\nFOO=bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "trailing_hash_is_part_of_value",
			in:   "FOO=bar # not a comment\n",
			want: []string{"FOO=bar # not a comment"},
		},
		{
			name: "blank_lines_skipped",
			in:   "\n\n\nFOO=bar\n\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "missing_eq_skipped",
			in:   "this line has no equals sign\nFOO=bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "invalid_key_skipped",
			in:   "1FOO=bar\nFOO-BAR=baz\nFOO=ok\n",
			want: []string{"FOO=ok"},
		},
		{
			name: "line_continuation_unquoted",
			in:   "FOO=line1\\\nline2\n",
			want: []string{"FOO=line1line2"},
		},
		{
			name: "line_continuation_inside_double_quotes",
			in:   "FOO=\"line1\\\nline2\"\n",
			want: []string{"FOO=line1line2"},
		},
		{
			name: "literal_double_backslash_at_eol",
			in:   "FOO=foo\\\\\nBAR=bar\n",
			want: []string{`FOO=foo\\`, "BAR=bar"},
		},
		{
			name: "empty_value",
			in:   "FOO=\n",
			want: []string{"FOO="},
		},
		{
			name: "empty_double_quoted_value",
			in:   "FOO=\"\"\n",
			want: []string{"FOO="},
		},
		{
			name: "empty_single_quoted_value",
			in:   "FOO=''\n",
			want: []string{"FOO="},
		},
		{
			name: "underscore_key",
			in:   "_FOO=bar\nFOO_BAR_BAZ=qux\n",
			want: []string{"_FOO=bar", "FOO_BAR_BAZ=qux"},
		},
		{
			name: "multiple_entries",
			in:   "A=1\nB=2\nC=3\n",
			want: []string{"A=1", "B=2", "C=3"},
		},
		{
			name: "bom_at_start_tolerated",
			in:   "\ufeffFOO=bar\n",
			want: []string{"FOO=bar"},
		},
		{
			name: "no_trailing_newline",
			in:   "FOO=bar",
			want: []string{"FOO=bar"},
		},
		{
			name: "trailing_continuation_with_no_next_line",
			in:   "FOO=bar\\",
			want: []string{"FOO=bar"},
		},
		{
			name: "single_quoted_with_double_quote_inside",
			in:   `FOO='he said "hi"'` + "\n",
			want: []string{`FOO=he said "hi"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvFileReader(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestParseEnvironmentFileErrors covers cases that should fail parsing
// rather than silently produce a partial result.
func TestParseEnvironmentFileErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "unterminated_double_quote",
			in:   `FOO="unterminated` + "\n",
			want: "unterminated double-quoted",
		},
		{
			name: "unterminated_single_quote",
			in:   "FOO='unterminated\n",
			want: "unterminated single-quoted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEnvFileReader(strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want contains %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseEnvironmentFileFromDisk pins the file-shape API: open from
// disk, return os.IsNotExist verbatim for the "ignore-if-missing"
// caller path.
func TestParseEnvironmentFileFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")
	body := "RESOLUTION=1280x800\nCOL_DEPTH=24\nMAX_FRAME_RATE=24\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvironmentFile(path)
	if err != nil {
		t.Fatalf("ParseEnvironmentFile: %v", err)
	}
	want := []string{"RESOLUTION=1280x800", "COL_DEPTH=24", "MAX_FRAME_RATE=24"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}

	// Missing file surfaces os.IsNotExist for the caller to interpret.
	_, err = ParseEnvironmentFile(filepath.Join(dir, "absent"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}

// TestEnvfileCorpus loads every fixture under testdata/envfile/ and
// asserts the parser produces the fixture's matching .want list.
func TestEnvfileCorpus(t *testing.T) {
	entries, err := os.ReadDir("testdata/envfile")
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	saw := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		envPath := filepath.Join("testdata/envfile", e.Name())
		wantPath := strings.TrimSuffix(envPath, ".env") + ".want"
		t.Run(e.Name(), func(t *testing.T) {
			got, err := ParseEnvironmentFile(envPath)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			wantBytes, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatalf("read .want: %v", err)
			}
			var want []string
			for _, line := range strings.Split(strings.TrimRight(string(wantBytes), "\n"), "\n") {
				if line != "" {
					want = append(want, line)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("\n got: %#v\nwant: %#v", got, want)
			}
		})
		saw++
	}
	if saw == 0 {
		t.Errorf("no corpus fixtures under testdata/envfile/ — at least one expected")
	}
}

// FuzzParseEnvironmentFile is the safety-net target for env-file
// parsing: the parser must never panic on arbitrary bytes. Seeded with
// the table cases plus a handful of pathological inputs known to break
// quote-aware tokenisers in subtle ways.
func FuzzParseEnvironmentFile(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"\x00\n",
		"\ufeffFOO=bar\n",
		"FOO=bar\n",
		`FOO='single'` + "\n",
		`FOO="double\n\t"` + "\n",
		"FOO=line1\\\nline2\n",
		"FOO=\"line1\\\nline2\"\n",
		"# comment\n; comment\nFOO=bar\n",
		"FOO=foo'bar'\"baz\"\n",
		"FOO=\"unterminated",
		"FOO='unterminated",
		"FOO=\\\\\\\n",
		`FOO="he said \"hi\""` + "\n",
		strings.Repeat("FOO=bar\n", 100),
		"\\\n\\\n\\\nFOO=bar\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		// Bound inputs so the corpus doesn't grow gigabytes during a
		// long fuzz run; the parser is byte-rate-bounded so this is
		// purely a corpus-size guard.
		if len(input) > 1<<16 {
			t.Skip()
		}
		_, _ = parseEnvFileReader(strings.NewReader(input))
	})
}
