package pyre

import "github.com/dlclark/regexp2/v2"

// Compiles a Python re pattern whose MatchString answers re.fullmatch over the whole subject
func CompileFullmatch(pattern string) (*regexp2.Regexp, error) {
	out, err := Translate(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(`\A(?:`+out+`)\z`, regexp2.None)
	if err != nil {
		return nil, &Error{Msg: err.Error(), Pattern: pattern}
	}
	return re, nil
}
