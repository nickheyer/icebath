package jsonschemautil

import (
	"time"

	"github.com/dlclark/regexp2/v2"
)

// Readings a pattern is tried in, in order: ECMA-262 with the Unicode flag, ECMA-262 without it, the engine's own syntax with inline flags and named groups, and the RE2 syntax with its (?P<name>...) groups and (?P=name) back references
var patternModes = [...]regexp2.RegexOptions{regexp2.ECMAScript | regexp2.Unicode, regexp2.ECMAScript, regexp2.None, regexp2.RE2}

// Regular expression read in the first of patternModes that accepts it, the error of the ECMA-262 reading when none does; each match limited to timeout when timeout is positive
func compilePattern(pattern string, timeout time.Duration) (*regexp2.Regexp, error) {
	var first error
	for _, mode := range patternModes {
		re, err := regexp2.Compile(pattern, mode)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if timeout > 0 {
			re.MatchTimeout = timeout
		}
		return re, nil
	}
	return nil, first
}

// Whether a string is a regular expression compilePattern accepts, the regex format
func validPattern(s string) bool {
	_, err := compilePattern(s, 0)
	return err == nil
}
