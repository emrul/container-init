package unit

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// ParseEnvironmentFile reads an EnvironmentFile= target and returns
// its entries in "KEY=VALUE" form, ready to merge into a process
// environment slice. Semantics match systemd's env-file.c:
//
//   - One assignment per logical line; logical lines fold trailing
//     "\<newline>" into a continuation.
//   - Lines that are empty or begin with '#' / ';' (after leading
//     whitespace) are skipped.
//   - Lines without '=' are skipped silently, where systemd warns.
//   - Keys must match [A-Za-z_][A-Za-z0-9_]*; otherwise the line is
//     skipped.
//   - Values are a sequence of unquoted, '...'-quoted, and "..."-quoted
//     segments. Adjacent segments concatenate. Leading and trailing
//     whitespace is stripped from unquoted segments at the line level.
//   - Inside '...' the value is literal -- no escape processing.
//   - Inside "..." the recognised escapes are \" \\ \n \t \r \a \b \f
//     \v \' \$ \space; everything else is preserved verbatim. As in
//     systemd, $VAR / ${VAR} are not expanded; ${VAR} expansion
//     applies only to directive values, such as the EnvironmentFile=
//     path itself.
//
// An error from opening the file is returned verbatim, so callers can
// honour EnvFileRef.IgnoreMissing with os.IsNotExist.
func ParseEnvironmentFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseEnvFileReader(f)
}

// parseEnvFileReader is ParseEnvironmentFile over any reader.
func parseEnvFileReader(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	// 1 MiB max logical line (systemd's is 32 KiB): continuation
	// lines can stack arbitrarily.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		entries []string
		joined  strings.Builder
	)
	firstLine := true
	for sc.Scan() {
		line := sc.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		// Line continuation joins the next physical line, whether the
		// trailing "\" is inside or outside quotes, as in systemd.
		if strings.HasSuffix(line, "\\") && !endsWithEscapedBackslash(line) {
			joined.WriteString(line[:len(line)-1])
			continue
		}
		joined.WriteString(line)
		full := joined.String()
		joined.Reset()
		if kv, ok, err := parseEntry(full); err != nil {
			return nil, err
		} else if ok {
			entries = append(entries, kv)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if joined.Len() > 0 {
		if kv, ok, err := parseEntry(joined.String()); err != nil {
			return nil, err
		} else if ok {
			entries = append(entries, kv)
		}
	}
	return entries, nil
}

// endsWithEscapedBackslash returns true when the trailing "\" was
// preceded by an odd-length run of "\". A logical "\\" pair at line
// end is a literal backslash, not a line continuation.
func endsWithEscapedBackslash(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		n++
	}
	return n%2 == 0
}

// parseEntry takes a complete logical line (continuation already
// resolved) and returns its "KEY=VALUE" form, an ok flag for skip
// decisions, and any fatal error.
func parseEntry(line string) (string, bool, error) {
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	if trimmed == "" {
		return "", false, nil
	}
	if trimmed[0] == '#' || trimmed[0] == ';' {
		return "", false, nil
	}
	eq := strings.IndexByte(trimmed, '=')
	if eq < 0 {
		return "", false, nil
	}
	key := strings.TrimRightFunc(trimmed[:eq], unicode.IsSpace)
	if !validKey(key) {
		return "", false, nil
	}
	val, err := parseEnvValue(trimmed[eq+1:])
	if err != nil {
		return "", false, fmt.Errorf("env-file: key %s: %w", key, err)
	}
	return key + "=" + val, true, nil
}

// validKey enforces the [A-Za-z_][A-Za-z0-9_]* rule.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// parseEnvValue handles the value-side of KEY=VALUE. Concatenates a
// sequence of unquoted, '...'-quoted, and "..."-quoted segments;
// strips leading/trailing whitespace at the line level (not within
// quotes).
func parseEnvValue(s string) (string, error) {
	// Skip leading whitespace.
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var b strings.Builder
	quotedEnd := 0 // trailing whitespace before this is quoted: keep it
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			seg, n, err := readDQuoted(s[i:])
			if err != nil {
				return "", err
			}
			b.WriteString(seg)
			quotedEnd = b.Len()
			i += n
		case '\'':
			seg, n, err := readSQuoted(s[i:])
			if err != nil {
				return "", err
			}
			b.WriteString(seg)
			quotedEnd = b.Len()
			i += n
		default:
			start := i
			for i < len(s) && s[i] != '"' && s[i] != '\'' {
				i++
			}
			b.WriteString(s[start:i])
		}
	}
	v := b.String()
	return v[:quotedEnd] + strings.TrimRightFunc(v[quotedEnd:], unicode.IsSpace), nil
}

// readDQuoted parses one "..." segment starting at s[0]='"'. Returns
// the unescaped content, the number of bytes consumed (both quotes
// included), and any error.
func readDQuoted(s string) (string, int, error) {
	if len(s) == 0 || s[0] != '"' {
		return "", 0, fmt.Errorf(`expected "`)
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), i + 1, nil
		}
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte(0x07)
			case 'b':
				b.WriteByte(0x08)
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '"', '\\', '\'', '$', ' ':
				b.WriteByte(next)
			default:
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			i++
			continue
		}
		b.WriteByte(c)
	}
	return "", 0, fmt.Errorf("unterminated double-quoted value")
}

// readSQuoted parses one '...' segment. No escape processing -- the
// content is literal up to the matching closing quote.
func readSQuoted(s string) (string, int, error) {
	if len(s) == 0 || s[0] != '\'' {
		return "", 0, fmt.Errorf("expected '")
	}
	end := strings.IndexByte(s[1:], '\'')
	if end < 0 {
		return "", 0, fmt.Errorf("unterminated single-quoted value")
	}
	return s[1 : 1+end], 2 + end, nil
}
