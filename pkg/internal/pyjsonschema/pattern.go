package pyjsonschema

import "github.com/dlclark/regexp2/v2"

// Search predicate of an ECMA style schema pattern, re.search semantics
func compilePattern(p string) (func(string) bool, error) {
	re, err := regexp2.Compile(p, regexp2.ECMAScript)
	if err != nil {
		re, err = regexp2.Compile(p, regexp2.None)
		if err != nil {
			return nil, err
		}
	}
	return func(s string) bool { ok, _ := re.MatchString(s); return ok }, nil
}
