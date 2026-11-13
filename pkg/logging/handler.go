package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const (
	logFormat        = "%(asctime)s %(levelname)s%(session_tag)s %(name)s: %(message)s"
	logFormatVerbose = "%(asctime)s - %(name)s - %(levelname)s%(session_tag)s - %(message)s"
)

var processStart = time.Now()

// Renders records through a Python logging format, redacting the whole line when built with the redaction hook
type Formatter struct {
	format  string
	datefmt string
	redact  slog.Handler
}

// Formatter for a printf style format and strftime date format, the default message format when empty
func NewFormatter(format, datefmt string) *Formatter {
	if format == "" {
		format = "%(message)s"
	}
	return &Formatter{format: format, datefmt: datefmt}
}

// Formatter whose output passes through the redaction hook, the RedactingFormatter form
func newRedactingFormatter(format, datefmt string) (*Formatter, error) {
	if RedactingHandler == nil {
		return nil, errRedactUnlinked
	}
	f := NewFormatter(format, datefmt)
	f.redact = RedactingHandler(captureSink{})
	return f, nil
}

// Redacting formatter the agent, errors, gateway and gui log files write their lines through
func LogFileFormatter() (*Formatter, error) { return newRedactingFormatter(logFormat, "") }

// Timestamp text, strftime of datefmt or the default date with comma separated milliseconds
func formatTime(t time.Time, datefmt string) string {
	t = t.Local()
	if datefmt != "" {
		return pydatetime.Strftime(t, datefmt)
	}
	return fmt.Sprintf("%s,%03d", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6)
}

// Formatted line for rec, redacted under the profile policy ctx carries
func (f *Formatter) Format(ctx context.Context, rec *Record) (string, error) {
	var frame *runtime.Frame
	caller := func() runtime.Frame {
		if frame == nil {
			fr, _ := runtime.CallersFrames([]uintptr{rec.PC}).Next()
			frame = &fr
		}
		return *frame
	}
	created := float64(rec.Created.UnixNano()) / 1e9
	lookup := func(key string) (any, bool) {
		switch key {
		case "name":
			return rec.Name, true
		case "levelno":
			return rec.Levelno, true
		case "levelname":
			return rec.Levelname, true
		case "message", "msg":
			return rec.Msg, true
		case "asctime":
			return formatTime(rec.Created, f.datefmt), true
		case "created":
			return created, true
		case "msecs":
			return float64(rec.Created.Nanosecond() / 1e6), true
		case "relativeCreated":
			return float64(rec.Created.Sub(processStart).Nanoseconds()) / 1e6, true
		case "session_tag":
			return rec.SessionTag, true
		case "hermes_home":
			return rec.HermesHome, true
		case "process":
			return os.Getpid(), true
		case "processName":
			return "MainProcess", true
		case "thread":
			return int64(rec.Thread), true
		case "threadName":
			return fmt.Sprintf("Goroutine-%d", rec.Thread), true
		case "taskName", "args", "exc_info", "exc_text", "stack_info":
			return nil, true
		case "pathname":
			return caller().File, true
		case "filename":
			return filepath.Base(caller().File), true
		case "module":
			return strings.TrimSuffix(filepath.Base(caller().File), ".go"), true
		case "funcName":
			fn := caller().Function
			return fn[strings.LastIndexByte(fn, '.')+1:], true
		case "lineno":
			return caller().Line, true
		}
		return nil, false
	}
	line, err := pyrepr.PercentMap(f.format, lookup)
	if err != nil {
		var pe *pyrepr.PercentError
		if errors.As(err, &pe) && pe.Class == "KeyError" {
			return "", fmt.Errorf("ValueError: Formatting field not found in record: %s", pe.Msg)
		}
		return "", err
	}
	if f.redact == nil {
		return line, nil
	}
	var out struct {
		text string
		ok   bool
	}
	r := slog.NewRecord(rec.Created, toSlog(rec.Levelno), line, rec.PC)
	if err := f.redact.Handle(context.WithValue(ctx, captureKey{}, &out), r); err != nil {
		return "", err
	}
	if !out.ok {
		return "", errRedactDropped
	}
	return out.text, nil
}

type captureKey struct{}

// Terminal handler of the redaction chain, storing the redacted line for the formatter that asked
type captureSink struct{}

func (captureSink) Enabled(context.Context, slog.Level) bool { return true }

func (captureSink) Handle(ctx context.Context, r slog.Record) error {
	if out, ok := ctx.Value(captureKey{}).(*struct {
		text string
		ok   bool
	}); ok {
		out.text, out.ok = r.Message, true
	}
	return nil
}

func (s captureSink) WithAttrs([]slog.Attr) slog.Handler { return s }

func (s captureSink) WithGroup(string) slog.Handler { return s }

// slog level standing for a Python level number
func toSlog(level int) slog.Level { return slog.Level((level - 20) * 2 / 5) }

// Python level number of a slog level, Debug 10, Info 20, Warn 30, Error 40
func fromSlog(level slog.Level) int { return 20 + int(level)*5/2 }

// Level, filters, formatter and the reentrant I/O lock every handler shares
type handlerCore struct {
	lock      pythread.RLock
	mu        sync.Mutex
	level     int
	filters   []Filter
	formatter *Formatter
}

// Minimum level the handler emits
func (c *handlerCore) Level() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.level
}

// Sets the minimum level the handler emits
func (c *handlerCore) SetLevel(level int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.level = level
}

// Adds a filter every record must pass
func (c *handlerCore) AddFilter(f Filter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters = append(c.filters, f)
}

// Sets the formatter used to render records
func (c *handlerCore) SetFormatter(f *Formatter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.formatter = f
}

// Formatter in use, the plain message formatter when none was set
func (c *handlerCore) Formatter() *Formatter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.formatter == nil {
		return NewFormatter("", "")
	}
	return c.formatter
}

// Reports whether rec passes every filter
func (c *handlerCore) filter(rec *Record) bool {
	c.mu.Lock()
	fs := append([]Filter(nil), c.filters...)
	c.mu.Unlock()
	for _, f := range fs {
		if !f.Filter(rec) {
			return false
		}
	}
	return true
}

// Prints the logging error report to stderr the way Handler.handleError does
func handleErrorBase(rec *Record, err error) {
	fmt.Fprintf(os.Stderr, "--- Logging error ---\n%s: %s\nMessage: %s\nArguments: ()\n", errorClass(err), errorText(err), pyrepr.StrRepr(rec.Msg))
}

// Python exception class name an error stands for
func errorClass(err error) string {
	var pe *pyrepr.PercentError
	if errors.As(err, &pe) {
		return pe.Class
	}
	var lt *lockTimeoutError
	if errors.As(err, &lt) {
		return "RuntimeError"
	}
	if errors.Is(err, os.ErrClosed) {
		return "ValueError"
	}
	if isOSError(err) {
		return pypath.OSErrorClass(err)
	}
	if cls, _, ok := strings.Cut(err.Error(), ": "); ok && strings.HasSuffix(cls, "Error") && !strings.ContainsAny(cls, " .") {
		return cls
	}
	return "Exception"
}

// Python str of the exception an error stands for
func errorText(err error) string {
	var pe *pyrepr.PercentError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	if errors.Is(err, os.ErrClosed) {
		return "I/O operation on closed file."
	}
	if isOSError(err) {
		return pypath.OSErrorText(err)
	}
	if cls, msg, ok := strings.Cut(err.Error(), ": "); ok && strings.HasSuffix(cls, "Error") && !strings.ContainsAny(cls, " .") {
		return msg
	}
	return err.Error()
}

// Reports whether err is an operating system error Python raises as OSError
func isOSError(err error) bool {
	var errno syscall.Errno
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	return errors.As(err, &errno) || errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrPermission)
}

// Handler writing formatted records to a stream, the verbose console handler
type streamHandler struct {
	handlerCore
	w       io.Writer
	verbose bool
}

// Stream handler over w registered for Shutdown
func newStreamHandler(w io.Writer) *streamHandler {
	h := &streamHandler{w: w}
	registerHandler(h)
	return h
}

func (h *streamHandler) Handle(ctx context.Context, rec *Record) bool {
	if !h.filter(rec) {
		return false
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	msg, err := h.Formatter().Format(ctx, rec)
	if err == nil {
		_, err = io.WriteString(h.w, msg+"\n")
	}
	if err != nil {
		handleErrorBase(rec, err)
	}
	return true
}

func (h *streamHandler) Close() error {
	unregisterHandler(h)
	return nil
}
