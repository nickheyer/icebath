package app

import (
	"regexp"
)

// ptSearchDirection is which way a buffer search walks
type ptSearchDirection int

// Search directions
const (
	ptSearchForward ptSearchDirection = iota
	ptSearchBackward
)

// ptSearchState is a search query with its direction and case rule
type ptSearchState struct {
	text       string
	direction  ptSearchDirection
	ignoreCase bool
}

var ptLeadingWhitespaceRe = regexp.MustCompile(`^[` + ptPyWhitespace + `]*`)
