package pyhost

import (
	"errors"
	"strings"
	"sync"
)

// PyError is a Python exception carried into Go with its type, message and formatted traceback
type PyError struct {
	Type      string
	Message   string
	Traceback string
	exc       *Ref
}

func (e *PyError) Error() string { return e.Type + ": " + e.Message }

// Short type name, the qualified name without its module
func (e *PyError) Name() string { return e.Type[strings.LastIndex(e.Type, ".")+1:] }

// Exception is a Go error raised into Python as the named class, a builtin name or module.Class
type Exception struct {
	Type    string
	Message string
}

func (e *Exception) Error() string { return e.Type + ": " + e.Message }

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
		return &PyError{Type: "SystemError", Message: "error return without exception set"}
	}
	return g.describe(g.adopt(exc))
}

// Builds a PyError from an exception object the way traceback.format_exception prints it
func (g *GIL) describe(exc *Ref) *PyError {
	e := &PyError{exc: exc}
	typ := pyObjectType(exc.obj)
	defer pyDecRef(typ)
	e.Type = g.qualifiedName(typ)
	if s := pyObjectStr(exc.obj); s != 0 {
		e.Message, _ = g.unicode(s)
		pyDecRef(s)
	} else {
		pyErrClear()
		e.Message = "<exception str() failed>"
	}
	e.Traceback = g.formatTraceback(typ, exc.obj)
	return e
}

func (g *GIL) qualifiedName(typ uintptr) string {
	name := "?"
	if q := pyObjectGetAttrString(typ, "__qualname__"); q != 0 {
		name, _ = g.unicode(q)
		pyDecRef(q)
	} else {
		pyErrClear()
	}
	if m := pyObjectGetAttrString(typ, "__module__"); m != 0 {
		mod, _ := g.unicode(m)
		pyDecRef(m)
		if mod != "" && mod != "builtins" {
			name = mod + "." + name
		}
	} else {
		pyErrClear()
	}
	return name
}

// Text of traceback.format_exception for the exception, the formatting failure itself when it raises
func (g *GIL) formatTraceback(typ, exc uintptr) string {
	tbmod := pyImportImportModule("traceback")
	if tbmod == 0 {
		return "traceback module import failed: " + g.takeErrorText()
	}
	defer pyDecRef(tbmod)
	tb := pyObjectGetAttrString(exc, "__traceback__")
	if tb == 0 {
		return "exception has no __traceback__: " + g.takeErrorText()
	}
	defer pyDecRef(tb)
	lines, err := g.CallMethod(tbmod, "format_exception", []any{Borrowed(typ), Borrowed(exc), Borrowed(tb)}, nil)
	if err != nil {
		return "traceback.format_exception raised " + err.Error()
	}
	defer pyDecRef(lines)
	v, err := g.FromPython(lines)
	if err != nil {
		return "traceback lines did not convert: " + err.Error()
	}
	var b strings.Builder
	for _, l := range v.([]any) {
		b.WriteString(l.(string))
	}
	return b.String()
}

func (g *GIL) takeErrorText() string {
	if pyErrOccurred() == 0 {
		return "no exception set"
	}
	var t, v, tb uintptr
	pyErrFetch(&t, &v, &tb)
	text := "?"
	if v != 0 {
		if s := pyObjectStr(v); s != 0 {
			text, _ = g.unicode(s)
			pyDecRef(s)
		}
	}
	for _, o := range []uintptr{t, v, tb} {
		if o != 0 {
			pyDecRef(o)
		}
	}
	pyErrClear()
	return text
}

var (
	classMu sync.Mutex
	classes = map[string]uintptr{}
)

// Exception class named by a builtin name or a module.Class path, held for the process lifetime
func (g *GIL) exceptionClass(name string) (uintptr, error) {
	classMu.Lock()
	defer classMu.Unlock()
	if c, ok := classes[name]; ok {
		return c, nil
	}
	module, attr := "builtins", name
	if i := strings.LastIndex(name, "."); i >= 0 {
		module, attr = name[:i], name[i+1:]
	}
	m, err := g.Import(module)
	if err != nil {
		return 0, err
	}
	defer pyDecRef(m)
	c, err := g.Attr(m, attr)
	if err != nil {
		return 0, err
	}
	classes[name] = c
	return c, nil
}

func excAttributeError(g *GIL) uintptr {
	c, err := g.exceptionClass("AttributeError")
	if err != nil {
		return excRuntimeError
	}
	return c
}

// Raises err in Python, re-raising the original exception object when it came from Python
func (g *GIL) raise(err error) {
	var pe *PyError
	if errors.As(err, &pe) && pe.exc != nil {
		pyIncRef(pe.exc.obj)
		if hasRaisedException {
			pyErrSetRaisedException(pe.exc.obj)
		} else {
			typ := pyObjectType(pe.exc.obj)
			tb := pyObjectGetAttrString(pe.exc.obj, "__traceback__")
			pyErrRestore(typ, pe.exc.obj, tb)
		}
		return
	}
	var ex *Exception
	if errors.As(err, &ex) {
		c, cerr := g.exceptionClass(ex.Type)
		if cerr == nil {
			pyErrSetString(c, ex.Message)
			return
		}
		pyErrSetString(excRuntimeError, ex.Type+": "+ex.Message+" (exception class unavailable: "+cerr.Error()+")")
		return
	}
	pyErrSetString(excRuntimeError, err.Error())
}
