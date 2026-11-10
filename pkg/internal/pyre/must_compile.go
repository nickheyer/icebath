package pyre

import "github.com/dlclark/regexp2/v2"

// Compiled pattern with Python semantics, panicking on a pattern Python rejects
func MustCompile(pattern string) *regexp2.Regexp {
	re, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}
