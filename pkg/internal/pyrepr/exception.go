package pyrepr

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Error declaring the Python exception class it stands for
type PyTypeNamed interface{ PyTypeName() string }

var exceptionPrefixRE = regexp.MustCompile(`^([A-Z][A-Za-z0-9_]*(?:Error|Exception|Exit|Interrupt|Warning)): `)

// Python class name of err, a leading class prefix in the text naming it and plain errors standing for Exception
func ExceptionName(err error) string {
	if err == nil {
		return "NoneType"
	}
	var named PyTypeNamed
	if errors.As(err, &named) && named.PyTypeName() != "" {
		return named.PyTypeName()
	}
	var fe *FormatError
	if errors.As(err, &fe) && fe.Class != "" {
		return fe.Class
	}
	if m := exceptionPrefixRE.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	name := fmt.Sprintf("%T", err)
	switch name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return "Exception"
	}
	name = strings.TrimLeft(name, "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Python str of the exception, the message without the class prefix the Go text carries
func ExceptionMessage(err error) string {
	if err == nil {
		return "None"
	}
	var fe *FormatError
	if errors.As(err, &fe) && fe.Class != "" {
		return fe.Msg
	}
	text := err.Error()
	if m := exceptionPrefixRE.FindStringSubmatch(text); m != nil {
		return text[len(m[0]):]
	}
	return text
}

// Python repr of the exception, the class name around the repr of its message or empty parentheses
func ExceptionRepr(err error) string {
	if err == nil {
		return "None"
	}
	if msg := ExceptionMessage(err); msg != "" {
		return ExceptionName(err) + "(" + Repr(msg) + ")"
	}
	return ExceptionName(err) + "()"
}
