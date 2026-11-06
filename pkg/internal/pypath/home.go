package pypath

import "strings"

// RuntimeError pathlib raises when no home directory can be determined
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *RuntimeError) PyTypeName() string { return "RuntimeError" }

func errNoHome() error { return &RuntimeError{Msg: "Could not determine home directory."} }

// pathlib Path.home of the host, expanduser of a bare tilde failing when it stays unexpanded
func PathHome() (string, error) {
	h := Expanduser("~")
	if h == "~" {
		return "", errNoHome()
	}
	return PathStr(h), nil
}

// pathlib Path.expanduser of the host, a relative path whose first part starts with a tilde gaining its home
func PathExpanduser(p string) (string, error) {
	anchor, parts := PathParts(p)
	if anchor != "" || len(parts) == 0 || !strings.HasPrefix(parts[0], "~") {
		return PathStr(p), nil
	}
	h := Expanduser(parts[0])
	if strings.HasPrefix(h, "~") {
		return "", errNoHome()
	}
	return PathJoin(h, parts[1:]...), nil
}
