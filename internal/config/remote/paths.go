package remote

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Key of the config plane as its segments
type Path []string

// Key the plane refuses: one that does not parse or breaks a limit of its contract
type PathError struct{ Message string }

func (e *PathError) Error() string { return e.Message }

// Limits of a key on the plane
const (
	maxSegments     = 32
	maxSegmentBytes = 256
	maxEncodedBytes = 2048
)

// Key as the plane writes it: backslash and dot escaped in each segment, the segments joined by dots
func Encode(segments []string) string {
	out := make([]string, len(segments))
	for i, s := range segments {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), ".", `\.`)
	}
	return strings.Join(out, ".")
}

// Segments of a key as the plane writes it, a PathError for a bad escape, an empty segment or a broken limit
func Decode(encoded string) (Path, error) {
	var segments []string
	var current strings.Builder
	empty := true
	runes := []rune(encoded)
	for i := 0; i < len(runes); i++ {
		switch ch := runes[i]; ch {
		case '\\':
			if i+1 >= len(runes) || (runes[i+1] != '\\' && runes[i+1] != '.') {
				return nil, &PathError{fmt.Sprintf("The key %s can't be read. A backslash in a key is followed by a dot or by another backslash.", strconv.Quote(encoded))}
			}
			current.WriteRune(runes[i+1])
			empty = false
			i++
		case '.':
			if empty {
				return nil, &PathError{fmt.Sprintf("The key %s has an empty part between its dots.", strconv.Quote(encoded))}
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
		return nil, &PathError{fmt.Sprintf("The key %s has an empty part between its dots.", strconv.Quote(encoded))}
	}
	segments = append(segments, current.String())
	if err := validate(segments, encoded); err != nil {
		return nil, err
	}
	return segments, nil
}

// Segment count, byte lengths, control characters and the reserved segment the plane refuses
func validate(segments []string, shown string) error {
	if len(segments) < 1 || len(segments) > maxSegments {
		return &PathError{fmt.Sprintf("The key %s must have from 1 to %d parts.", strconv.Quote(shown), maxSegments)}
	}
	if len(shown) > maxEncodedBytes {
		return &PathError{fmt.Sprintf("The key is longer than %s.", strs.Size(float64(maxEncodedBytes)))}
	}
	for _, seg := range segments {
		if len(seg) < 1 || len(seg) > maxSegmentBytes {
			return &PathError{fmt.Sprintf("Each part of the key %s must be from 1 to %s long.", strconv.Quote(shown), strs.Size(float64(maxSegmentBytes)))}
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7F {
				return &PathError{fmt.Sprintf("The key %s holds a character that can't be shown.", strconv.Quote(shown))}
			}
		}
		if seg == "__proto__" {
			return &PathError{fmt.Sprintf("The key %s can't have a part named __proto__.", strconv.Quote(shown))}
		}
	}
	return nil
}

// Whether the lock covers the path: its segments start the path's
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

// Key a level above the profile holds for itself, with the name of the level
type Lock struct {
	Path  Path
	Level string
}

// First lock covering the path, false when none does
func covering(locks []Lock, path []string) (Lock, bool) {
	for _, l := range locks {
		if Covers(l.Path, path) {
			return l, true
		}
	}
	return Lock{}, false
}

// Path of a dotted key as icebath spells it, split on every dot when it does not parse as the plane writes keys
func FromDotted(dotted string) Path {
	if p, err := Decode(dotted); err == nil {
		return p
	}
	return strings.Split(dotted, ".")
}
