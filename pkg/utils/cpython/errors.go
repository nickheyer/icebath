package cpythonutil

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

var (
	// Returned for work asked of a host once shutdown has begun
	ErrClosed = errors.New("embedded Python host is shut down")
	// Start called after an interpreter was started in this process, running or finalized, since the process-wide object caches belong to that one
	ErrStarted = errors.New("an embedded Python interpreter was already started in this process")
	// Async work asked of a host whose asyncio loop thread is not running
	ErrNoLoop = errors.New("the host asyncio loop is not running")
	// Finalize called on a thread other than the one Start locked
	ErrWrongThread = errors.New("finalize must run on the goroutine that called Start")
	// A Go value with no Python form or a Python object with no Go form
	ErrUnconvertible = errors.New("value has no conversion")
	// Work that needs other OS threads asked of a child created by os.fork
	ErrForkedChild = errors.New("this call needs Go runtime threads, which do not exist in a child created by os.fork, use multiprocessing spawn or forkserver")
	// Start on a platform where purego cannot call libpython or take callbacks from it, matched by errors.ErrUnsupported too
	ErrUnsupported = fmt.Errorf("no embedded CPython support for %s/%s: %w", runtime.GOOS, runtime.GOARCH, errors.ErrUnsupported)
)

// Python exception carried into Go with its qualified class name and message, the exception instance kept for re-raising and matching
type Error struct {
	Type    string
	Message string
	exc     *Ref
	tb      atomic.Pointer[string]
}

// Class and message the way the last line of a Python traceback shows them, the class alone for an empty message
func (e *Error) Error() string {
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// Short class name, the qualified name without its module
func (e *Error) Name() string { return e.Type[strings.LastIndex(e.Type, ".")+1:] }

// The exception instance the error carries, nil for an error made in Go
func (e *Error) Exception() *Ref { return e.exc }

// Text traceback.format_exception gives for the exception, formatted under the GIL on first use, empty for an error without an instance
func (e *Error) Traceback() string {
	if e.exc == nil {
		return ""
	}
	if text := e.tb.Load(); text != nil {
		return *text
	}
	var text string
	if err := e.exc.h.Do(func(g *GIL) error { text = g.formatTraceback(e.exc.obj); return nil }); err != nil {
		return "traceback unavailable: " + err.Error()
	}
	e.tb.CompareAndSwap(nil, &text)
	return *e.tb.Load()
}

// Go error raised into Python as the named class, a builtin name or module.Class
type Exception struct {
	Type    string
	Message string
}

// Class and message, the class alone for an empty message
func (e *Exception) Error() string {
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// C API function or data symbol the loaded libpython does not export
type SymbolError struct {
	Name string
	Err  error
}

// The missing symbol and the loader's reason when it gave one
func (e *SymbolError) Error() string {
	if e.Err == nil {
		return "libpython lacks " + e.Name
	}
	return "libpython lacks " + e.Name + ": " + e.Err.Error()
}

// Loader failure behind the missing symbol
func (e *SymbolError) Unwrap() error { return e.Err }

// Failed step of interpreter initialization with the C function and message CPython reported, or the exit it asked for
type InitError struct {
	Op       string
	Func     string
	Message  string
	Exit     bool
	ExitCode int
}

// The step and why it failed
func (e *InitError) Error() string {
	switch {
	case e.Exit:
		return fmt.Sprintf("%s: interpreter requested exit with code %d", e.Op, e.ExitCode)
	case e.Func == "":
		return e.Op + ": " + e.Message
	}
	return e.Op + ": " + e.Func + ": " + e.Message
}

// Takes the pending Python exception and describes it
func (g *GIL) fetchError() error {
	var exc uintptr
	if hasRaisedException {
		exc = pyErrGetRaisedException()
	} else {
		var t, v, tb uintptr
		pyErrFetch(&t, &v, &tb)
		pyErrNormalizeException(&t, &v, &tb)
		if v != 0 && tb != 0 {
			pyExceptionSetTraceback(v, tb)
		}
		if t != 0 {
			pyDecRef(t)
		}
		if tb != 0 {
			pyDecRef(tb)
		}
		exc = v
	}
	if exc == 0 {
		return &Error{Type: "SystemError", Message: "error return without exception set"}
	}
	return g.describe(g.adopt(exc))
}

// Error for an exception instance the Ref owns, its traceback left for Traceback to format on demand
func (g *GIL) describe(exc *Ref) *Error {
	e := &Error{exc: exc, Type: g.qualifiedName(pyType(exc.obj))}
	if s := pyObjectStr(exc.obj); s != 0 {
		e.Message, _ = g.unicode(s)
		pyDecRef(s)
	} else {
		pyErrClear()
		e.Message = "<exception str() failed>"
	}
	return e
}

// module.qualname of a class, the bare qualname for builtins; the class is held across both lookups, which can run Python code that reassigns the __class__ it was read from
func (g *GIL) qualifiedName(typ uintptr) string {
	pyIncRef(typ)
	defer pyDecRef(typ)
	name := "?"
	if q := g.optionalAttr(typ, "__qualname__"); q != 0 {
		if s, err := g.unicode(q); err == nil {
			name = s
		}
		pyDecRef(q)
	}
	if m := g.optionalAttr(typ, "__module__"); m != 0 {
		if mod, err := g.unicode(m); err == nil && mod != "" && mod != "builtins" {
			name = mod + "." + name
		}
		pyDecRef(m)
	}
	return name
}

// Text of traceback.format_exception for the exception, the formatting failure itself when it raises
func (g *GIL) formatTraceback(exc uintptr) string {
	format := g.h.formatException.Load()
	if format == 0 {
		mod, err := g.Import("traceback")
		if err != nil {
			return "traceback module import failed: " + err.Error()
		}
		f, err := g.Attr(mod, "format_exception")
		pyDecRef(mod)
		if err != nil {
			return "traceback.format_exception is missing: " + err.Error()
		}
		if !g.h.formatException.CompareAndSwap(0, f) {
			pyDecRef(f)
		}
		format = g.h.formatException.Load()
	}
	tb := g.optionalAttr(exc, "__traceback__")
	if tb == 0 {
		return "exception has no __traceback__"
	}
	defer pyDecRef(tb)
	// The class is held, as the old PyObject_Type reference was: building the argument tuple can run the collector and with it finalizers
	typ := pyType(exc)
	pyIncRef(typ)
	defer pyDecRef(typ)
	lines, err := g.Call(format, []any{Borrowed(typ), Borrowed(exc), Borrowed(tb)}, nil)
	if err != nil {
		return "traceback.format_exception raised " + err.Error()
	}
	defer pyDecRef(lines)
	items, err := g.FromPython(lines)
	if err != nil {
		return "traceback lines did not convert: " + err.Error()
	}
	list, _ := items.([]any)
	var b strings.Builder
	for _, l := range list {
		text, _ := l.(string)
		b.WriteString(text)
	}
	return b.String()
}

// Sets the pending exception to cls with msg as its text, invalid UTF-8 in msg replaced, a MemoryError left pending when the text cannot be built
func (g *GIL) setError(cls uintptr, msg string) {
	if !utf8.ValidString(msg) {
		msg = strings.ToValidUTF8(msg, "\uFFFD")
	}
	p := stringPointer(msg)
	s := pyUnicodeFromStringAndSize(p, len(msg))
	if s == 0 {
		return
	}
	pyErrSetObject(cls, s)
	pyDecRef(s)
}

// Raises err in Python, re-raising the original exception object when it came from Python
func (g *GIL) raise(err error) {
	var pe *Error
	if errors.As(err, &pe) && pe.exc != nil {
		exc := pe.exc.obj
		pyIncRef(exc)
		if hasRaisedException {
			pyErrSetRaisedException(exc)
			return
		}
		typ := pyType(exc)
		pyIncRef(typ)
		pyErrRestore(typ, exc, g.optionalAttr(exc, "__traceback__"))
		return
	}
	var ex *Exception
	if errors.As(err, &ex) {
		c, cerr := g.exceptionClass(ex.Type)
		if cerr == nil {
			g.setError(c, ex.Message)
			return
		}
		g.setError(excRuntimeError, ex.Error()+" (exception class unavailable: "+cerr.Error()+")")
		return
	}
	g.setError(excRuntimeError, err.Error())
}

// Exception classes looked up by name, held for the process lifetime
var (
	classMu sync.Mutex
	classes = map[string]uintptr{}
)

// Exception class named by a builtin name or a module.Class path, borrowed from the process cache
func (g *GIL) exceptionClass(name string) (uintptr, error) {
	classMu.Lock()
	c, ok := classes[name]
	classMu.Unlock()
	if ok {
		return c, nil
	}
	// Resolved without the lock: an import runs Python code, which may hand the GIL to a thread waiting on classMu
	c, err := g.lookupClass(name)
	if err != nil {
		return 0, err
	}
	classMu.Lock()
	prev, ok := classes[name]
	if !ok {
		classes[name] = c
	}
	classMu.Unlock()
	if ok {
		pyDecRef(c)
		return prev, nil
	}
	return c, nil
}

// Class named by a builtin name or a module.Class path, a new reference
func (g *GIL) lookupClass(name string) (uintptr, error) {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return g.Attr(g.h.builtins, name)
	}
	m, err := g.Import(name[:i])
	if err != nil {
		return 0, err
	}
	defer pyDecRef(m)
	return g.Attr(m, name[i+1:])
}
