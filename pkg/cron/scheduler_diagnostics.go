package cron

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const workerStderrTailChars = 1500

// Run document error section with chained causes and URL credentials redacted
func formatRunError(s *Store, err error) string {
	chain := []error{}
	for e := err; e != nil; e = errors.Unwrap(e) {
		chain = append(chain, e)
	}
	var b strings.Builder
	for i := len(chain) - 1; i >= 0; i-- {
		b.WriteString(tracebackFrames(chain[i]))
		b.WriteString(exceptionOnlyLine(chain[i]))
		if i > 0 {
			b.WriteString("\nThe above exception was the direct cause of the following exception:\n\n")
		}
	}
	text := tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, b.String(), tools.RedactOptions{Force: true, URLCredentials: true})
	return "## Error\n\n```\n" + text + "\n```\n"
}

// Exception line as traceback prints it, the bare class name when the message is empty
func exceptionOnlyLine(err error) string {
	if msg := err.Error(); msg != "" {
		return cronExceptionName(err) + ": " + msg + "\n"
	}
	return cronExceptionName(err) + "\n"
}

// Redacted tail of an external worker's captured stderr as a message suffix, empty when nothing was captured
func externalWorkerStderrTail(s *Store, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := pystr.Strip(pystr.DecodeUTF8SigReplace(raw))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	tail := text
	if len(runes) > workerStderrTailChars {
		tail = "…" + string(runes[len(runes)-workerStderrTailChars:])
	}
	tail = tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, tail, tools.RedactOptions{Force: true, URLCredentials: true})
	return "; worker stderr: " + tail
}

// Error carrying the call frames active where it was raised, rendered as a Python traceback
type tracedError struct {
	name, msg string
	cause     error
	frames    []runtime.Frame
}

func (e *tracedError) Error() string                { return e.msg }
func (e *tracedError) Unwrap() error                { return e.cause }
func (e *tracedError) PyTypeName() string           { return e.name }
func (e *tracedError) StackFrames() []runtime.Frame { return e.frames }

// Error of an exception class raised at the caller with its cause and the caller frames recorded
func raiseTraced(name, msg string, cause error) error {
	return &tracedError{name: name, msg: msg, cause: cause, frames: callerFrames(3)}
}

// Error of a recovered panic value with the frames of the panicking goroutine
func panicTraceback(r any) error {
	if err, ok := r.(error); ok {
		return &tracedError{name: cronExceptionName(err), msg: err.Error(), cause: errors.Unwrap(err), frames: callerFrames(3)}
	}
	return &tracedError{name: "RuntimeError", msg: fmt.Sprint(r), frames: callerFrames(3)}
}

// Non runtime frames above skip, outermost first like a Python traceback
func callerFrames(skip int) []runtime.Frame {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip, pcs)
	iter := runtime.CallersFrames(pcs[:n])
	var frames []runtime.Frame
	for {
		f, more := iter.Next()
		if !strings.HasPrefix(f.Function, "runtime.") && f.Function != "" {
			frames = append([]runtime.Frame{f}, frames...)
		}
		if !more {
			return frames
		}
	}
}

// Traceback header and frame lines of an error that recorded its frames, empty otherwise
func tracebackFrames(err error) string {
	traced, ok := err.(interface{ StackFrames() []runtime.Frame })
	if !ok || len(traced.StackFrames()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Traceback (most recent call last):\n")
	for _, f := range traced.StackFrames() {
		name := f.Function[strings.LastIndex(f.Function, "/")+1:]
		if i := strings.Index(name, "."); i >= 0 {
			name = name[i+1:]
		}
		fmt.Fprintf(&b, "  File \"%s\", line %d, in %s\n", f.File, f.Line, name)
		if src := sourceLine(f.File, f.Line); src != "" {
			b.WriteString("    " + src + "\n")
		}
	}
	return b.String()
}

// Stripped text of one source line, empty when the file is unreadable
func sourceLine(path string, line int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return pystr.Strip(lines[line-1])
}
