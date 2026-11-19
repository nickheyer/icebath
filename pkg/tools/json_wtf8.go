package tools

import (
	"runtime"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python os.fsencode, surrogateescape on POSIX and unchanged on Windows where Go hands surrogates to UTF-16 APIs
func fsEncode(s string) (string, error) {
	if runtime.GOOS == "windows" || utf8.ValidString(s) {
		return s, nil
	}
	b, err := pystr.SurrogateEscapeEncode(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Error os.fsencode raises for s, nil when every surrogate names a host byte
func fsEncodeError(s string) error {
	_, err := fsEncode(s)
	return err
}
