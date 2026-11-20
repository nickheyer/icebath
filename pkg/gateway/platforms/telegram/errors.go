package telegram

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// TelegramError is a Bot API failure standing for the python-telegram-bot exception class it names
type TelegramError struct {
	Class      string
	Message    string
	RetryAfter float64
	NewChatID  int64
	Err        error
}

func (e *TelegramError) Error() string      { return e.Message }
func (e *TelegramError) Unwrap() error      { return e.Err }
func (e *TelegramError) PyTypeName() string { return e.Class }

// Parent class of each python-telegram-bot exception, BadRequest and TimedOut being network errors
var ptbErrorParents = map[string]string{
	"BadRequest": "NetworkError", "TimedOut": "NetworkError", "NetworkError": "TelegramError",
	"Forbidden": "TelegramError", "InvalidToken": "TelegramError", "Conflict": "TelegramError",
	"RetryAfter": "TelegramError", "ChatMigrated": "TelegramError", "EndPointNotFound": "TelegramError",
}

// Error of the class with the API prefix stripped and the rest capitalized when a prefix was cut
func newTelegramError(class, message string, cause error) *TelegramError {
	msg := message
	for _, prefix := range []string{"Error: ", "[Error]: ", "Bad Request: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if msg != message {
		msg = pystr.Capitalize(msg)
	}
	return &TelegramError{Class: class, Message: msg, Err: cause}
}

// Whether err itself is an instance of the python-telegram-bot class or one of its subclasses
func isPTBError(err error, class string) bool {
	te, ok := err.(*TelegramError)
	if !ok || te == nil {
		return false
	}
	for cur := te.Class; cur != ""; cur = ptbErrorParents[cur] {
		if cur == class {
			return true
		}
	}
	return false
}

var exceptionClassPrefixRE = regexp.MustCompile(`^([A-Z][A-Za-z0-9_]*(?:Error|Exception|Exit|Interrupt|Warning)): `)

// Python class name of err itself, never of an error it wraps
func className(err error) string {
	if err == nil {
		return "NoneType"
	}
	if named, ok := err.(pyrepr.PyTypeNamed); ok && named.PyTypeName() != "" {
		return named.PyTypeName()
	}
	if m := exceptionClassPrefixRE.FindStringSubmatch(err.Error()); m != nil {
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

// Whether err itself stands for a Python OSError, socket and timeout failures included
func isOSError(err error) bool {
	switch err.(type) {
	case *net.OpError, *net.DNSError, *net.AddrError, *os.PathError, *os.SyscallError, *os.LinkError, syscall.Errno:
		return true
	}
	if err == os.ErrDeadlineExceeded {
		return true
	}
	return slices.Contains(pyrepr.BuiltinExceptionMRO(className(err)), "OSError")
}

// Error and every wrapped ancestor in depth first order, each visited once even across cycles
func iterExceptionGraph(err error) []error {
	var out []error
	seen := map[any]bool{}
	stack := []error{err}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if isComparable(cur) {
			if seen[cur] {
				continue
			}
			seen[cur] = true
		}
		out = append(out, cur)
		switch u := cur.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				if x != nil {
					stack = append(stack, x)
				}
			}
		case interface{ Unwrap() error }:
			if x := u.Unwrap(); x != nil {
				stack = append(stack, x)
			}
		}
	}
	return out
}

// Whether an error value can key a map without panicking
func isComparable(err error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_ = map[any]bool{err: true}
	return true
}

// Polling startup fenced by teardown, internal control flow only
type pollingLifecycleAbort struct{ msg string }

func (e *pollingLifecycleAbort) Error() string      { return e.msg }
func (e *pollingLifecycleAbort) PyTypeName() string { return "_PollingLifecycleAbort" }

// Confirmed getUpdates stall from the watchdog or the post reconnect verifier, not a transport drop
type PollingStallError struct{ Msg string }

func (e *PollingStallError) Error() string      { return e.Msg }
func (e *PollingStallError) PyTypeName() string { return "_PollingStallError" }
