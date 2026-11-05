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

// Model text naming host bytes, each U+DC80 through U+DCFF its byte and other surrogates left for the OS call to refuse
func fsText(s string) string {
	if runtime.GOOS == "windows" || utf8.ValidString(s) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, size := pystr.DecodeRune(s[i:])
		if r >= pystr.SurrogateEscapeMin && r <= pystr.SurrogateEscapeMax {
			out = append(out, byte(r-pystr.SurrogateLowMin))
		} else {
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return string(out)
}
