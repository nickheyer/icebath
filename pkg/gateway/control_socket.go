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
		return false, errors.New("the control socket path is not valid UTF-8")
	}
	return len(path) <= maxUnixPath, nil
}
