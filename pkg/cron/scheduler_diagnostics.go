package cron

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const workerStderrTailChars = 1500

// Run document error section with the error and its causes, URL credentials redacted, the stack logged only
func formatRunError(s *Store, err error) string {
	var b strings.Builder
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e != err {
			b.WriteString("caused by ")
		}
		b.WriteString(exceptionOnlyLine(e))
		if frames := tracebackFrames(e); frames != "" {
			pkgLog().Error("cron run failed", "error", exceptionOnlyLine(e), "stack", frames)
		}
	}
	text := tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, b.String(), tools.RedactOptions{Force: true, URLCredentials: true})
	return "## Error\n\n```\n" + text + "\n```\n"
}

// Message of one error of the chain on its own line, a fixed phrase when the error has no message
func exceptionOnlyLine(err error) string {
	if msg := err.Error(); msg != "" {
		return msg + "\n"
	}
	return "an error with no message\n"
}

// Redacted tail of an external worker's captured stderr as a message suffix, empty when nothing was captured
func externalWorkerStderrTail(s *Store, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(strings.ToValidUTF8(string(textutil.TrimBOM(raw)), string(utf8.RuneError)))
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
func (e *tracedError) ErrorKind() string            { return e.name }
func (e *tracedError) StackFrames() []runtime.Frame { return e.frames }

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

// Stack lines of an error that recorded its frames, outermost first, empty otherwise
func tracebackFrames(err error) string {
	traced, ok := err.(interface{ StackFrames() []runtime.Frame })
	if !ok || len(traced.StackFrames()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("stack, outermost call first:\n")
	for _, f := range traced.StackFrames() {
		name := f.Function[strings.LastIndex(f.Function, "/")+1:]
		if i := strings.Index(name, "."); i >= 0 {
			name = name[i+1:]
		}
		fmt.Fprintf(&b, "  %s at %s:%d\n", name, f.File, f.Line)
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
	return strings.TrimSpace(lines[line-1])
}
