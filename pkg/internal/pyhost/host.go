package pyhost

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Options select the interpreter and the environment the embedded runtime starts with
type Options struct {
	Interpreter   *Interpreter
	PycachePrefix string
	ExtraPaths    []string
	Argv          []string
}

// Host owns the one embedded interpreter of the process
type Host struct {
	in         *Interpreter
	mainTS     uintptr
	closed     atomic.Bool
	inflight   atomic.Int64
	finalizing atomic.Bool
	reacquire  sync.Mutex

	relMu    sync.Mutex
	releases []uintptr

	loop        uintptr
	loopThread  uintptr
	isAwaitable uintptr
	asyncio     uintptr
	pendingMu   sync.Mutex
	pending     map[uint64]chan asyncResult
	pendingSeq  atomic.Uint64
}

// ErrClosed is returned once shutdown has begun
var ErrClosed = errors.New("python host is shut down")

var current *Host

// Object layout facts measured from live objects after initialization
var (
	obTypeOffset    uintptr
	objectBasicSize uintptr
	floatValOffset  uintptr
)

// Loads libpython, initializes it and leaves the calling goroutine locked to the OS thread that must call Finalize
func Start(opts Options) (*Host, error) {
	if current != nil {
		return nil, errors.New("python host already started in this process")
	}
	runtime.LockOSThread()
	h, err := start(opts)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	return h, nil
}

func start(opts Options) (*Host, error) {
	h := &Host{in: opts.Interpreter, pending: map[uint64]chan asyncResult{}}
	lookup, err := openLibrary(opts.Interpreter.Library)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", opts.Interpreter.Library, err)
	}
	if err := bind(lookup); err != nil {
		return nil, err
	}
	bindInitConfig(lookup)
	if pyIsInitialized() != 0 {
		return nil, errors.New("interpreter already initialized")
	}
	s := settingsFor(opts)
	if hasInitConfig {
		if err := initFromInitConfig(s); err != nil {
			return nil, err
		}
	} else if err := initFromLayout(opts.Interpreter, s); err != nil {
		return nil, err
	}
	resolveExceptionVariables()
	current = h
	ensureCallbacks()
	g := &GIL{h: h}
	if err := g.measureLayout(); err != nil {
		return nil, err
	}
	if err := g.prependSysPath(opts.ExtraPaths); err != nil {
		return nil, err
	}
	if err := g.startLoop(); err != nil {
		return nil, err
	}
	h.mainTS = pyEvalSaveThread()
	return h, nil
}

// Interpreter the host was started from
func (h *Host) Interpreter() *Interpreter { return h.in }

// GIL proves the goroutine holds an attached thread state of the interpreter
type GIL struct{ h *Host }

// Host the token belongs to
func (g *GIL) Host() *Host { return g.h }

// Runs fn with the GIL held on a locked OS thread, refusing new work once shutdown began
func (h *Host) Do(fn func(g *GIL) error) error {
	h.inflight.Add(1)
	defer h.inflight.Add(-1)
	if h.closed.Load() {
		return ErrClosed
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	state := pyGILStateEnsure()
	defer pyGILStateRelease(state)
	h.drainReleases()
	return fn(&GIL{h: h})
}

// Releases the GIL around blocking Go work, a thread finishing after finalization began never re-enters Python
func (g *GIL) Unblocked(fn func()) {
	ts := pyEvalSaveThread()
	fn()
	g.h.reacquire.Lock()
	if g.h.finalizing.Load() {
		g.h.reacquire.Unlock()
		select {}
	}
	pyEvalRestoreThread(ts)
	g.h.reacquire.Unlock()
}

func (h *Host) queueRelease(o uintptr) {
	h.relMu.Lock()
	h.releases = append(h.releases, o)
	h.relMu.Unlock()
}

func (h *Host) drainReleases() {
	h.relMu.Lock()
	batch := h.releases
	h.releases = nil
	h.relMu.Unlock()
	for _, o := range batch {
		pyDecRef(o)
	}
}

// Stops the loop, waits for in flight calls and finalizes, run on the goroutine that called Start
func (h *Host) Finalize(timeout time.Duration) (int, error) {
	h.closed.Store(true)
	deadline := time.Now().Add(timeout)
	for h.inflight.Load() != 0 {
		if time.Now().After(deadline) {
			return -1, fmt.Errorf("%d calls still inside Python after %s", h.inflight.Load(), timeout)
		}
		time.Sleep(time.Millisecond)
	}
	pyEvalRestoreThread(h.mainTS)
	g := &GIL{h: h}
	if err := g.stopLoop(deadline); err != nil {
		return -1, err
	}
	h.drainReleases()
	h.reacquire.Lock()
	h.finalizing.Store(true)
	h.reacquire.Unlock()
	rc := int(pyFinalizeEx())
	runtime.UnlockOSThread()
	return rc, nil
}

// Measures the ob_type offset, the base object size and the float payload offset from live objects
func (g *GIL) measureLayout() error {
	noneType := pyObjectType(pyNone)
	defer pyDecRef(noneType)
	for off := uintptr(0); off <= 64; off += unsafe.Sizeof(uintptr(0)) {
		if *(*uintptr)(cptr(pyNone + off)) == noneType && *(*uintptr)(cptr(pyTrue + off)) == pyBoolType {
			obTypeOffset = off
			break
		}
	}
	if obTypeOffset == 0 {
		return errors.New("ob_type offset not found in None and True")
	}
	size, err := g.basicSize(pyBaseObjectType)
	if err != nil {
		return err
	}
	objectBasicSize = size
	fsize, err := g.basicSize(pyFloatType)
	if err != nil {
		return err
	}
	floatValOffset = fsize - 8
	probe, err := g.ToPython(1.5)
	if err != nil {
		return err
	}
	defer pyDecRef(probe)
	if *(*float64)(cptr(probe + floatValOffset)) != 1.5 {
		return errors.New("float payload offset check failed")
	}
	return nil
}

func (g *GIL) basicSize(t uintptr) (uintptr, error) {
	v := pyObjectGetAttrString(t, "__basicsize__")
	if v == 0 {
		return 0, g.fetchError()
	}
	defer pyDecRef(v)
	return uintptr(pyLongAsUnsignedLongLong(v)), nil
}

func pyType(o uintptr) uintptr { return *(*uintptr)(cptr(o + obTypeOffset)) }

// Inserts directories at the front of sys.path in the given order
func (g *GIL) prependSysPath(paths []string) error {
	sysPath := pySysGetObject("path")
	if sysPath == 0 {
		return errors.New("sys.path missing")
	}
	for i := len(paths) - 1; i >= 0; i-- {
		p, err := g.ToPython(paths[i])
		if err != nil {
			return err
		}
		rc := pyListInsert(sysPath, 0, p)
		pyDecRef(p)
		if rc != 0 {
			return g.fetchError()
		}
	}
	return nil
}

// Imports a module, a new reference
func (g *GIL) Import(name string) (uintptr, error) {
	m := pyImportImportModule(name)
	if m == 0 {
		return 0, g.fetchError()
	}
	return m, nil
}

// Attribute lookup, a new reference
func (g *GIL) Attr(o uintptr, name string) (uintptr, error) {
	v := pyObjectGetAttrString(o, name)
	if v == 0 {
		return 0, g.fetchError()
	}
	return v, nil
}

// Whether the object has the attribute, lookup errors count as absent the way hasattr reports them
func (g *GIL) HasAttr(o uintptr, name string) bool { return pyObjectHasAttrString(o, name) == 1 }

// Sets an attribute from a Go value
func (g *GIL) SetAttr(o uintptr, name string, value any) error {
	v, err := g.ToPython(value)
	if err != nil {
		return err
	}
	defer pyDecRef(v)
	if pyObjectSetAttrString(o, name, v) != 0 {
		return g.fetchError()
	}
	return nil
}

// Calls callable with Go arguments, a new reference
func (g *GIL) Call(callable uintptr, args []any, kwargs map[string]any) (uintptr, error) {
	tuple, err := g.tuple(args)
	if err != nil {
		return 0, err
	}
	defer pyDecRef(tuple)
	var kw uintptr
	if len(kwargs) > 0 {
		if kw, err = g.ToPython(kwargs); err != nil {
			return 0, err
		}
		defer pyDecRef(kw)
	}
	r := pyObjectCall(callable, tuple, kw)
	if r == 0 {
		return 0, g.fetchError()
	}
	return r, nil
}

// Calls a method by name, a new reference
func (g *GIL) CallMethod(o uintptr, name string, args []any, kwargs map[string]any) (uintptr, error) {
	m, err := g.Attr(o, name)
	if err != nil {
		return 0, err
	}
	defer pyDecRef(m)
	return g.Call(m, args, kwargs)
}

// Decrements a reference obtained from this package
func (g *GIL) Decref(o uintptr) { pyDecRef(o) }

// Truth value of an object
func (g *GIL) Truthy(o uintptr) (bool, error) {
	switch pyObjectIsTrue(o) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Text of str(o)
func (g *GIL) Str(o uintptr) (string, error) {
	s := pyObjectStr(o)
	if s == 0 {
		return "", g.fetchError()
	}
	defer pyDecRef(s)
	return g.unicode(s)
}

// Text of repr(o)
func (g *GIL) Repr(o uintptr) (string, error) {
	s := pyObjectRepr(o)
	if s == 0 {
		return "", g.fetchError()
	}
	defer pyDecRef(s)
	return g.unicode(s)
}

// Whether the object is callable
func (g *GIL) Callable(o uintptr) bool { return pyCallableCheck(o) == 1 }

// Whether o is an instance of cls, with the exception of a failing check as the error
func (g *GIL) IsInstance(o, cls uintptr) (bool, error) {
	switch pyObjectIsInstance(o, cls) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Whether class o derives from cls, with the exception of a failing check as the error
func (g *GIL) IsSubclass(o, cls uintptr) (bool, error) {
	switch pyObjectIsSubclass(o, cls) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	}
	return false, g.fetchError()
}

// Borrowed sys.modules
func (g *GIL) SysModules() uintptr { return pySysGetObject("modules") }

// Stores a value in a dict under a string key
func (g *GIL) DictSet(d uintptr, key string, value any) error {
	k, err := g.ToPython(key)
	if err != nil {
		return err
	}
	defer pyDecRef(k)
	v, err := g.ToPython(value)
	if err != nil {
		return err
	}
	defer pyDecRef(v)
	if pyDictSetItem(d, k, v) != 0 {
		return g.fetchError()
	}
	return nil
}

// Keys of a dict as Go strings
func (g *GIL) DictKeys(d uintptr) ([]string, error) {
	var pos int
	var k, v uintptr
	var out []string
	for pyDictNext(d, &pos, &k, &v) != 0 {
		s, err := g.Str(k)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Whether inspect.isawaitable reports the object awaitable
func (g *GIL) IsAwaitable(o uintptr) (bool, error) {
	if g.h.isAwaitable == 0 {
		return false, errNoLoop
	}
	r, err := g.Call(g.h.isAwaitable, []any{Borrowed(o)}, nil)
	if err != nil {
		return false, err
	}
	defer pyDecRef(r)
	return r == pyTrue, nil
}

// Removes a string key from a dict, ignoring a missing key
func (g *GIL) DictDel(d uintptr, key string) {
	if pyDictDelItemString(d, key) != 0 {
		pyErrClear()
	}
}

// Borrowed item i of a tuple
func (g *GIL) TupleItem(t uintptr, i int) uintptr { return pyTupleGetItem(t, i) }

// New reference to None
func (g *GIL) None() uintptr {
	pyIncRef(pyNone)
	return pyNone
}

// Whether the pointer is the None singleton
func (g *GIL) IsNone(o uintptr) bool { return o == pyNone }

var startPid = syscall.Getpid()

// ErrForkedChild is returned for work that needs other OS threads inside a child created by os.fork
var ErrForkedChild = errors.New("this call needs Go runtime threads, which do not exist in a child created by os.fork, use multiprocessing spawn or forkserver")

// Whether the process is a child created by os.fork from plugin code
func Forked() bool { return syscall.Getpid() != startPid }
