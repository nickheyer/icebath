package remote

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxSegments     = 32
	maxSegmentBytes = 256
	maxEncodedBytes = 2048
)

var errPath = errors.New("invalid config path")

type lockEntry struct {
	path  []string
	level string
}

func quote(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\' || string(r) == q:
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

func encodePath(segments []string) string {
	parts := make([]string, len(segments))
	for i, s := range segments {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), ".", `\.`)
	}
	return strings.Join(parts, ".")
}

func decodePath(encoded string) ([]string, error) {
	var segments []string
	var current []byte
	for i := 0; i < len(encoded); i++ {
		ch := encoded[i]
		switch {
		case ch == '\\':
			if i+1 >= len(encoded) || (encoded[i+1] != '\\' && encoded[i+1] != '.') {
				return nil, fmt.Errorf("%w: invalid escape in config path %s", errPath, quote(encoded))
			}
			current = append(current, encoded[i+1])
			i++
		case ch == '.':
			if len(current) == 0 {
				return nil, fmt.Errorf("%w: empty segment in config path %s", errPath, quote(encoded))
			}
			segments = append(segments, string(current))
			current = nil
		default:
			current = append(current, ch)
		}
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("%w: empty segment in config path %s", errPath, quote(encoded))
	}
	segments = append(segments, string(current))
	if err := validatePath(segments, encoded); err != nil {
		return nil, err
	}
	return segments, nil
}

func validatePath(segments []string, encoded string) error {
	shown := quote(encoded)
	if len(segments) < 1 || len(segments) > maxSegments {
		return fmt.Errorf("%w: config path %s must have 1-%d segments", errPath, shown, maxSegments)
	}
	if len(encoded) > maxEncodedBytes {
		return fmt.Errorf("%w: config path is longer than %d bytes", errPath, maxEncodedBytes)
	}
	for _, seg := range segments {
		if len(seg) < 1 || len(seg) > maxSegmentBytes {
			return fmt.Errorf("%w: config path %s: a segment must be 1-%d bytes", errPath, shown, maxSegmentBytes)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: config path %s: control character in a segment", errPath, shown)
			}
		}
		if seg == "__proto__" {
			return fmt.Errorf("%w: reserved segment '__proto__' in config path %s", errPath, shown)
		}
	}
	return nil
}

func covers(lock, path []string) bool {
	if len(lock) > len(path) {
		return false
	}
	for i, s := range lock {
		if path[i] != s {
			return false
		}
	}
	return true
}

func samePath(a, b []string) bool {
	return len(a) == len(b) && covers(a, b)
}

func coveringLock(locks []lockEntry, path []string) (lockEntry, bool) {
	for _, l := range locks {
		if covers(l.path, path) {
			return l, true
		}
	}
	return lockEntry{}, false
}

func fromDotted(dotted string) []string {
	if p, err := decodePath(dotted); err == nil {
		return p
	}
	return strings.Split(dotted, ".")
}

func pathKey(p []string) string {
	return strings.Join(p, "\x00")
}
