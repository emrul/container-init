package supervisor

import (
	"bytes"
	"sync"
	"testing"
)

func TestLinePrefixWriter(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
		want   string
	}{
		{"single complete line", []string{"hello\n"}, "[u] hello\n"},
		{"multiple lines in one write", []string{"a\nb\nc\n"}, "[u] a\n[u] b\n[u] c\n"},
		{"split across writes", []string{"hel", "lo\nworld\n"}, "[u] hello\n[u] world\n"},
		{"trailing partial held back", []string{"first\npart"}, "[u] first\n"},
		{"empty line still tagged", []string{"\n"}, "[u] \n"},
		{"split exactly on newline", []string{"foo\n", "bar\n"}, "[u] foo\n[u] bar\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := newLinePrefixWriter("[u] ", &buf)
			for _, s := range tc.writes {
				if _, err := w.Write([]byte(s)); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			if buf.String() != tc.want {
				t.Errorf("got %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// TestLinePrefixWriterConcurrent confirms two goroutines writing
// distinct full lines do not interleave inside the same line. The
// mutex serialises the prefix+payload+newline write into a single
// underlying call.
func TestLinePrefixWriterConcurrent(t *testing.T) {
	var buf bytes.Buffer
	w := newLinePrefixWriter("[x] ", &buf)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = w.Write([]byte("aaa\n")) }()
		go func() { defer wg.Done(); _, _ = w.Write([]byte("bbb\n")) }()
	}
	wg.Wait()
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'}) {
		s := string(line)
		if s != "[x] aaa" && s != "[x] bbb" {
			t.Errorf("interleaved line: %q", s)
		}
	}
}

func TestUnitLabel(t *testing.T) {
	cases := map[string]string{
		"web.service":        "web",
		"audio-out-ws.socket": "audio-out-ws",
		"plain":              "plain",
	}
	for in, want := range cases {
		if got := unitLabel(in); got != want {
			t.Errorf("unitLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
