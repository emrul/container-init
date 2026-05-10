package supervisor

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// linePrefixWriter prefixes each '\n'-terminated line of its input
// with a fixed string before forwarding to the wrapped writer. A
// partial line (no trailing '\n') is buffered until the rest arrives,
// so prefixes are never injected mid-line.
//
// Concurrency: exec.Cmd starts independent io.Copy goroutines for
// stdout and stderr; the mutex keeps each write atomic at line
// granularity so concurrent stdout/stderr writes don't interleave
// inside a single line. Cross-unit interleaving is unchanged -- the
// kernel guarantees atomicity for writes ≤ PIPE_BUF (4096) on the
// pipe to journald, and our per-line writes are well under that.
type linePrefixWriter struct {
	prefix []byte
	w      io.Writer
	mu     sync.Mutex
	buf    bytes.Buffer
}

func newLinePrefixWriter(prefix string, w io.Writer) *linePrefixWriter {
	return &linePrefixWriter{prefix: []byte(prefix), w: w}
}

func (p *linePrefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := len(b)
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.buf.Write(b)
			return total, nil
		}
		line := make([]byte, 0, len(p.prefix)+p.buf.Len()+i+1)
		line = append(line, p.prefix...)
		line = append(line, p.buf.Bytes()...)
		line = append(line, b[:i+1]...)
		p.buf.Reset()
		if _, err := p.w.Write(line); err != nil {
			return 0, err
		}
		b = b[i+1:]
	}
}

// unitLabel strips the suffix from a unit name so log prefixes show
// "web" rather than "web.service".
func unitLabel(name string) string {
	for _, suf := range []string{".service", ".socket"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}
