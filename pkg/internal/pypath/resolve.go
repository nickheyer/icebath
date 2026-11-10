package pypath

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ValueError a path conversion raises before any system call, carrying the CPython message
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *ValueError) PyTypeName() string { return "ValueError" }

// pathlib Path.resolve of a WTF-8 str, failing where the lstat of a component cannot encode it
func Resolve(p string) (string, error) {
	p = PathStr(p)
	if windows {
		resolved, err := Realpath(p, Lax)
		if err != nil {
			return "", err
		}
		return PathStr(resolved), nil
	}
	encoded, err := pystr.SurrogateEscapeEncode(p)
	if err == nil && !strings.ContainsRune(p, 0) {
		resolved, err := Realpath(string(encoded), Lax)
		if err != nil {
			return "", err
		}
		return PathStr(resolved), nil
	}
	return "", unencodableComponent(p)
}

// Error the lstat of the first unencodable component raises, positioned within the path realpath built so far
func unencodableComponent(p string) error {
	parts := strings.Split(p, "/")
	for i, name := range parts {
		if _, err := pystr.SurrogateEscapeEncode(name); err == nil && !strings.ContainsRune(name, 0) {
			continue
		}
		prefix := strings.Join(parts[:i], "/")
		if prefix == "" && strings.HasPrefix(p, "/") {
			prefix = "/"
		}
		resolved := "/"
		if prefix != "/" {
			encoded, _ := pystr.SurrogateEscapeEncode(prefix)
			var err error
			if resolved, err = Realpath(string(encoded), Lax); err != nil {
				return err
			}
		}
		newpath := pystr.SurrogateEscapeDecode([]byte(resolved))
		if newpath != "/" {
			newpath += "/"
		}
		if _, err := pystr.SurrogateEscapeEncode(newpath + name); err != nil {
			return err
		}
		return &ValueError{Msg: "lstat: embedded null character in path"}
	}
	return nil
}
