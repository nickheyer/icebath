// Package errchain names the Go errors a failure wraps, the form icebath logs failures in
package errchain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"strings"
)

// Type and message of err and of every error it wraps, outermost first, joined errors in their order
func Chain(err error) []string {
	return walk(err, func(e error) string { return fmt.Sprintf("%T: %s", e, e.Error()) })
}

// Type of err and of every error it wraps, outermost first
func Types(err error) string {
	return strings.Join(walk(err, func(e error) string { return fmt.Sprintf("%T", e) }), " -> ")
}

// One entry per error of the chain in wrapping order
func walk(err error, entry func(error) string) []string {
	var out []string
	var visit func(e error)
	visit = func(e error) {
		if e == nil {
			return
		}
		out = append(out, entry(e))
		switch wrapped := e.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				visit(inner)
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return out
}

// Chain of err on one line, each wrapped error after an arrow
func Text(err error) string {
	return strings.Join(Chain(err), " -> ")
}

// Plain name of the kind of a decoded value such as text, a number, a list or a mapping
func Kind(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case bool:
		return "true or false"
	case []byte:
		return "bytes"
	case interface{ Float64() (float64, error) }:
		return "a number"
	case interface{ Get(string) (any, bool) }:
		return "a mapping"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return "text"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map:
		return "a mapping"
	case reflect.Func:
		return "a function"
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "nothing"
		}
		return Kind(rv.Elem().Interface())
	}
	return fmt.Sprintf("a %T", v)
}

// Reason of an operating system failure followed by the path, paths or command it concerns
func OSText(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) {
		if errors.Is(ee.Err, exec.ErrNotFound) {
			return "command not found: " + ee.Name
		}
		return ee.Err.Error() + ": " + ee.Name
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error() + ": " + le.Old + " -> " + le.New
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error() + ": " + pe.Path
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return se.Syscall + ": " + se.Err.Error()
	}
	return err.Error()
}
