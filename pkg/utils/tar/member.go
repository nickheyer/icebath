package tarutil

import (
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Member name that is empty, absolute, on a drive or share, or climbing out through a dot-dot element
var ErrUnsafePath = errors.New("unsafe member path")

// Member that would land, or link, outside the extraction directory
var ErrOutsideDest = errors.New("member leads outside the destination")

// Member of a kind extraction does not create: a device, a fifo, or a link where links are refused
var ErrUnsupportedType = errors.New("unsupported member type")

// Member whose content would not fit in the space left on the destination's volume, the guard against decompression bombs
var ErrTooLarge = errors.New("member does not fit in the space left on the destination")

// Archive member refused by its name, kind or size, wrapping ErrUnsafePath, ErrOutsideDest, ErrUnsupportedType or ErrTooLarge
type MemberError struct {
	Name string
	Err  error
}

// The member and the reason, as in archive member "../x": unsafe member path
func (e *MemberError) Error() string {
	return "archive member " + strconv.Quote(e.Name) + ": " + e.Err.Error()
}

// The reason
func (e *MemberError) Unwrap() error { return e.Err }

// Relative path elements of an archive member name split at / and \, empty and . elements dropped; a name that is empty, absolute, on a drive or share, has no element left or holds a .. element fails with a *MemberError wrapping ErrUnsafePath
func MemberParts(name string) ([]string, error) {
	normalized := strings.ReplaceAll(name, `\`, "/")
	var parts []string
	for p := range strings.SplitSeq(normalized, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	if strings.HasPrefix(normalized, "/") || hasDrive(name) || len(parts) == 0 || slices.Contains(parts, "..") {
		return nil, &MemberError{Name: name, Err: ErrUnsafePath}
	}
	return parts, nil
}

// Whether name starts with a drive letter and colon or a share's double separator
func hasDrive(name string) bool {
	if len(name) < 2 {
		return false
	}
	return name[1] == ':' || ((name[0] == '/' || name[0] == '\\') && (name[1] == '/' || name[1] == '\\'))
}

// Whether a member name names the archive root itself, such as . or ./
func isRootName(name string) bool {
	normalized := strings.ReplaceAll(name, `\`, "/")
	if normalized == "" || strings.HasPrefix(normalized, "/") {
		return false
	}
	for p := range strings.SplitSeq(normalized, "/") {
		if p != "" && p != "." {
			return false
		}
	}
	return true
}
