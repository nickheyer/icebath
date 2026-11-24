package dlutil

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Error for a platform this build has no dynamic library loader binding for
var ErrUnsupported = errors.New("no dynamic library loader for " + runtime.GOOS + "/" + runtime.GOARCH)

// Error for a library that no name, path or search turned up
var ErrNotFound = errors.New("library not found")

// Error for a build that has no trampoline through which C can call a Go function
var ErrNoCallback = errors.New("no C callback support for " + runtime.GOOS + "/" + runtime.GOARCH)

var errNotOpen = errors.New("library is not open")

// Callback the platform refused to make, most often because the process already made as many as it allows
type callbackError struct{ reason any }

// What the platform said when it refused
func (e *callbackError) Error() string { return fmt.Sprintf("new C callback: %v", e.reason) }

// Shared library that could not be opened, with the reason the platform loader gave
type LoadError struct {
	Name string
	Err  error
}

// Loader's reason, led by the library name when the reason does not already mention it
func (e *LoadError) Error() string { return withSubject(errText(e.Err, "cannot be opened"), e.Name) }

// Loader's own error
func (e *LoadError) Unwrap() error { return e.Err }

// Function or variable a library does not export, with the reason the platform loader gave
type SymbolError struct {
	Library string
	Symbol  string
	Err     error
}

// Loader's reason, naming the symbol and the library when the reason does not already mention them
func (e *SymbolError) Error() string {
	msg := errText(e.Err, "not found")
	if e.Symbol != "" && !strings.Contains(msg, e.Symbol) {
		msg = joinText("symbol "+e.Symbol, msg)
	}
	return withSubject(msg, e.Library)
}

// Loader's own error
func (e *SymbolError) Unwrap() error { return e.Err }

// One loaded shared library, kept loaded for the life of the process
type Library struct {
	name   string
	handle uintptr
}

// Binds a Go function variable to the library's function of that name, a SymbolError when the library lacks it
func (l *Library) Bind(fptr any, name string) error {
	addr, err := l.Symbol(name)
	if err != nil {
		return err
	}
	RegisterFunc(fptr, addr)
	return nil
}

// Name or path the library was opened by, empty for a nil library
func (l *Library) label() string {
	if l == nil {
		return ""
	}
	return l.name
}

// Text of an error, the fallback when there is none
func errText(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	if msg := err.Error(); msg != "" {
		return msg
	}
	return fallback
}

func joinText(head, tail string) string {
	if tail == "" {
		return head
	}
	return head + ": " + tail
}

// Message led by its subject unless the message already names it
func withSubject(msg, subject string) string {
	if subject == "" || strings.Contains(msg, subject) {
		return msg
	}
	return joinText(subject, msg)
}
