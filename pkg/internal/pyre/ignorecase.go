package pyre

import "github.com/dlclark/regexp2/v2"

// Compiles a Python re pattern under re.IGNORECASE with Python matching semantics
func CompileIgnoreCase(pattern string) (*regexp2.Regexp, error) {
	out, err := Translate(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(out, regexp2.IgnoreCase)
	if err != nil {
		return nil, &Error{Msg: err.Error(), Pattern: pattern}
	}
	return re, nil
}
