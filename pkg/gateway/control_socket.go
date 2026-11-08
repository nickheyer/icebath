package gateway

import (
	"errors"
	"unicode/utf8"
)

// sun_path limit is 104 on macOS and BSD and 108 on Linux, leaving margin for the NUL
const maxUnixPath = 100

// Whether a socket path fits sun_path once UTF-8 encoded, an error for undecodable bytes str.encode refuses
func fitsSunPath(path string) (bool, error) {
	if !utf8.ValidString(path) {
		return false, errors.New("'utf-8' codec can't encode characters: surrogates not allowed")
	}
	return len(path) <= maxUnixPath, nil
}

// Whether value is the namespaced auth v1 login form, the only identity a same user peer may forward a ticket for
func structuredLoginSubject(value string) bool {
	_, ok := ParseAuthenticatedSubject(value)
	return ok
}
