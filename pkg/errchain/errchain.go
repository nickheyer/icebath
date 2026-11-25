// Package errchain names the Go errors a failure wraps, the form icebath logs failures in
package errchain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"strconv"
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
	case *big.Int, *big.Float:
		return "a number"
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

// Value as a person writes it, text as is, plain numbers, true or false, list items joined by commas
func Show(v any) string {
	switch x := v.(type) {
	case nil:
		return "nothing"
	case string:
		if x == "" {
			return "empty text"
		}
		return x
	case []byte:
		return Show(string(x))
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case error:
		return x.Error()
	case interface{ TextOf() string }:
		return x.TextOf()
	case json.Marshaler:
		if b, err := x.MarshalJSON(); err == nil {
			return string(b)
		}
	case fmt.Stringer:
		return x.String()
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Len() == 0 {
			return "an empty list"
		}
		parts := make([]string, rv.Len())
		for i := range parts {
			parts[i] = Show(rv.Index(i).Interface())
		}
		return strings.Join(parts, ", ")
	case reflect.Map:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "nothing"
		}
		return Show(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

// Failure reading a plain message while the original stays underneath for the log
type reworded struct {
	msg string
	err error
}

func (e *reworded) Error() string { return e.msg }
func (e *reworded) Unwrap() error { return e.err }

// Error saying the formatted message to the reader, err kept in the chain for the log and class checks
func Reword(err error, format string, args ...any) error {
	return &reworded{msg: fmt.Sprintf(format, args...), err: err}
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
