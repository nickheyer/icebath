package remote

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Config plane path as its mapping key segments
type Path []string

// Encoded path that does not parse or breaks a segment or length limit
type PathError struct{ Message string }

func (e *PathError) Error() string { return e.Message }

const (
	MaxSegments     = 32
	MaxSegmentBytes = 256
	MaxEncodedBytes = 2048
)

// Wire string of a path, backslash and dot escaped in each segment and segments joined by dots
func Encode(segments []string) string {
	out := make([]string, len(segments))
	for i, s := range segments {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), ".", `\.`)
	}
	return strings.Join(out, ".")
}

// Segments of an encoded path, a PathError for a bad escape, an empty segment or a broken limit
func Decode(encoded string) (Path, error) {
	var segments []string
	var current strings.Builder
	empty := true
	runes := []rune(encoded)
	for i := 0; i < len(runes); i++ {
		switch ch := runes[i]; ch {
		case '\\':
			if i+1 >= len(runes) || (runes[i+1] != '\\' && runes[i+1] != '.') {
				return nil, &PathError{fmt.Sprintf("invalid escape in config path %s", pyrepr.StrRepr(encoded))}
			}
			current.WriteRune(runes[i+1])
			empty = false
			i++
		case '.':
			if empty {
				return nil, &PathError{fmt.Sprintf("empty segment in config path %s", pyrepr.StrRepr(encoded))}
			}
			segments = append(segments, current.String())
			current.Reset()
			empty = true
		default:
			current.WriteRune(ch)
			empty = false
		}
	}
	if empty {
		return nil, &PathError{fmt.Sprintf("empty segment in config path %s", pyrepr.StrRepr(encoded))}
	}
	segments = append(segments, current.String())
	if err := Validate(segments, &encoded); err != nil {
		return nil, err
	}
	return segments, nil
}

// Segment count, byte length, control character and reserved segment checks of the plane contract
func Validate(segments []string, encoded *string) error {
	shown := Encode(segments)
	if encoded != nil {
		shown = *encoded
	}
	if len(segments) < 1 || len(segments) > MaxSegments {
		return &PathError{fmt.Sprintf("config path %s must have 1-%d segments", pyrepr.StrRepr(shown), MaxSegments)}
	}
	if len(shown) > MaxEncodedBytes {
		return &PathError{fmt.Sprintf("config path is longer than %d bytes", MaxEncodedBytes)}
	}
	for _, seg := range segments {
		if len(seg) < 1 || len(seg) > MaxSegmentBytes {
			return &PathError{fmt.Sprintf("config path %s: a segment must be 1-%d bytes", pyrepr.StrRepr(shown), MaxSegmentBytes)}
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7F {
				return &PathError{fmt.Sprintf("config path %s: control character in a segment", pyrepr.StrRepr(shown))}
			}
		}
		if seg == "__proto__" {
			return &PathError{fmt.Sprintf("reserved segment '__proto__' in config path %s", pyrepr.StrRepr(shown))}
		}
	}
	return nil
}

// Reports whether a lock is a segment wise prefix of a path
func Covers(lock, path []string) bool {
	if len(lock) > len(path) {
		return false
	}
	for i := range lock {
		if lock[i] != path[i] {
			return false
		}
	}
	return true
}

// One lock with the level it holds
type Lock struct {
	Path  Path
	Level string
}

// First lock covering a path, false when none does
func CoveringLock(locks []Lock, path []string) (Lock, bool) {
	for _, l := range locks {
		if Covers(l.Path, path) {
			return l, true
		}
	}
	return Lock{}, false
}

// Path of a dotted key as callers spell it, split on every dot when it does not parse as encoded
func FromDotted(dotted string) Path {
	if p, err := Decode(dotted); err == nil {
		return p
	}
	return strings.Split(dotted, ".")
}
