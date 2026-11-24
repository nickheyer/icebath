package errorutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"syscall"
)

// Failure carrying a kind label beside its message
type Error struct{ Kind, Msg string }

// The message
func (e *Error) Error() string { return e.Msg }

// The kind label
func (e *Error) ErrorKind() string { return e.Kind }

type kinded interface{ ErrorKind() string }

// Label the first error in the chain declares through ErrorKind, else the Go type name of the first error that is not a plain errors.New, fmt.Errorf or errors.Join value, "error" when the chain holds only those, "" for nil
func Kind(err error) string {
	if err == nil {
		return ""
	}
	if label := declaredKind(err); label != "" {
		return label
	}
	if name := typeName(err); name != "" {
		return name
	}
	return "error"
}

// Whether err comes from the operating system: a path, link, syscall or errno error in the chain, a program that could not be started, or an error that reports itself as not existing, already existing or not permitted
func IsSystem(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	var errno syscall.Errno
	var execErr *exec.Error
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) || errors.As(err, &errno) || errors.As(err, &execErr) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrPermission)
}

// Non-empty label of the first error in a depth-first walk of the chain that declares one
func declaredKind(err error) string {
	for err != nil {
		if k, ok := err.(kinded); ok {
			if label := k.ErrorKind(); label != "" {
				return label
			}
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				if label := declaredKind(inner); label != "" {
					return label
				}
			}
			return ""
		default:
			return ""
		}
	}
	return ""
}

// Type name of the first error in a depth-first walk of the chain whose type is not a plain wrapper
func typeName(err error) string {
	for err != nil {
		if t := reflect.TypeOf(err); !plainError(t) {
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if name := t.Name(); name != "" {
				return name
			}
			return t.String()
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				if name := typeName(inner); name != "" {
					return name
				}
			}
			return ""
		default:
			return ""
		}
	}
	return ""
}

var (
	plainText     = reflect.TypeOf(errors.New(""))
	plainJoin     = reflect.TypeOf(errors.Join(errors.New("")))
	plainWrap     = reflect.TypeOf(fmt.Errorf("%w", errors.New("")))
	plainWrapMany = reflect.TypeOf(fmt.Errorf("%w%w", errors.New(""), errors.New("")))
)

// Whether t is the type errors.New, errors.Join or fmt.Errorf gives back
func plainError(t reflect.Type) bool {
	return t == plainText || t == plainJoin || t == plainWrap || t == plainWrapMany
}
