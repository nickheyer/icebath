package cpythonutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Interpreter and environment the embedded runtime starts with
type Options struct {
	Interpreter   *Interpreter
	PycachePrefix string
	ExtraPaths    []string
	Environment   bool
	Argv          []string
}

// The one embedded interpreter of the process
type Host struct {
	in        *Interpreter
	mainTS    uintptr
	closed    atomic.Bool
	finalized atomic.Bool
	inflight  atomic.Int64
	idle      chan struct{}

	finalizing atomic.Bool
	reacquire  sync.RWMutex

	relMu    sync.Mutex
	releases []uintptr
	queued   atomic.Bool

	builtins        uintptr
	formatException atomic.Uintptr

	asyncio      uintptr
	loop         uintptr
	loopThread   uintptr
	isAwaitable  uintptr
	ensureFuture uintptr
	pendingMu    sync.Mutex
	pending      map[uint64]chan asyncResult
	pendingSeq   atomic.Uint64
	settled      chan struct{}
}

var (
	startMu  sync.Mutex
	current  atomic.Pointer[Host]
	startPid atomic.Int64
)

// Object layout facts measured from live objects after initialization
var (
	obTypeOffset    uintptr
	objectBasicSize uintptr
)

// Whether the interpreter is a free-threaded build, where an attached thread state excludes no other thread
var freeThreaded bool

// Loads libpython and initializes it once per process, leaving the calling goroutine locked to the OS thread that must call Finalize
func Start(opts Options) (*Host, error) {
	if !supported {
		return nil, ErrUnsupported
	}
	if opts.Interpreter == nil {
		return nil, errors.New("cpython: Options.Interpreter is nil")
	}
	startMu.Lock()
	defer startMu.Unlock()
	if current.Load() != nil {
		return nil, ErrStarted
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
	in := opts.Interpreter
	lookup, err := openLibrary(in.Library)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", in.Library, err)
	}
	if err := bind(lookup); err != nil {
		return nil, err
	}
	bindInitConfig(lookup)
	if pyIsInitialized() != 0 {
		return nil, fmt.Errorf("%w: libpython reports it is already initialized", ErrStarted)
	}
	s := settingsFor(opts)
	if hasInitConfig {
		err = initFromInitConfig(s)
	} else {
		err = initFromLayout(in, s)
	}
	if err != nil {
		return nil, err
	}
	resolveExceptionVariables()
	freeThreaded = in.Macros["Py_GIL_DISABLED"]
	h := &Host{in: in, idle: make(chan struct{}, 1), pending: map[uint64]chan asyncResult{}, settled: make(chan struct{}, 1)}
	current.Store(h)
	startPid.Store(int64(os.Getpid()))
	ensureCallbacks()
	g := &GIL{h: h}
	if err := g.setup(opts.ExtraPaths); err != nil {
		// The interpreter is up but unusable: finalize it here rather than leave it holding the GIL on a thread the scheduler takes back, the host marked shut down first so an exception the error carries never reaches the finalized interpreter through Do
		h.closed.Store(true)
		h.finalized.Store(true)
		pyFinalizeEx()
		return nil, err
	}
	h.mainTS = pyEvalSaveThread()
	return h, nil
}

// Measures object layouts, keeps the builtins module, extends sys.path and starts the loop thread
func (g *GIL) setup(paths []string) error {
	if err := g.measureLayout(); err != nil {
		return err
	}
	builtins, err := g.Import("builtins")
	if err != nil {
		return err
	}
	g.h.builtins = builtins
	if err := g.prependSysPath(paths); err != nil {
		return err
	}
	return g.startLoop()
}

// The interpreter the host was started from
func (h *Host) Interpreter() *Interpreter { return h.in }

// Runs fn with the GIL held on a locked OS thread, refusing new work once shutdown began
func (h *Host) Do(fn func(g *GIL) error) error {
	h.inflight.Add(1)
	defer h.leave()
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

// Ends one Do, waking Finalize when the last call leaves after shutdown began
func (h *Host) leave() {
	if h.inflight.Add(-1) == 0 && h.closed.Load() {
		select {
		case h.idle <- struct{}{}:
		default:
		}
	}
}

// Queues a reference for the next GIL entry to drop, nothing once Py_FinalizeEx is under way since the reference goes with the interpreter
func (h *Host) queueRelease(o uintptr) {
	if h.finalized.Load() {
		return
	}
	h.relMu.Lock()
	h.releases = append(h.releases, o)
	h.queued.Store(true)
	h.relMu.Unlock()
}

// Drops the references queued by Release and by collected Refs, the GIL held
func (h *Host) drainReleases() {
	if !h.queued.Load() {
		return
	}
	h.relMu.Lock()
	batch := h.releases
	h.releases = nil
	h.queued.Store(false)
	h.relMu.Unlock()
	for _, o := range batch {
		pyDecRef(o)
	}
}

// Stops the loop, waits for in flight calls and finalizes; runs on the goroutine that called Start, ctx bounding every wait before Py_FinalizeEx
func (h *Host) Finalize(ctx context.Context) (int, error) {
	if h.finalized.Load() {
		return -1, ErrClosed
	}
	if pyGILStateGetThisThreadState() != h.mainTS {
		return -1, ErrWrongThread
	}
	h.closed.Store(true)
	for h.inflight.Load() != 0 {
		select {
		case <-h.idle:
		case <-ctx.Done():
			return -1, fmt.Errorf("%d calls still inside Python: %w", h.inflight.Load(), ctx.Err())
		}
	}
	pyEvalRestoreThread(h.mainTS)
	stopped := false
	defer func() {
		// A failed or panicking stopLoop leaves the interpreter running: its thread state is saved again so other threads and a retry find the GIL free
		if !stopped {
			h.mainTS = pyEvalSaveThread()
		}
	}()
	g := &GIL{h: h}
	if err := g.stopLoop(ctx); err != nil {
		return -1, err
	}
	stopped = true
	h.drainReleases()
	// Threads parked in Unblocked waiting for the GIL hold the read lock, so the GIL is let go while the flag flips
	ts := pyEvalSaveThread()
	h.reacquire.Lock()
	h.finalizing.Store(true)
	h.reacquire.Unlock()
	pyEvalRestoreThread(ts)
	h.finalized.Store(true)
	rc := int(pyFinalizeEx())
	runtime.UnlockOSThread()
	return rc, nil
}

// Measures the ob_type offset and the base object size from live objects
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
	return nil
}

func (g *GIL) basicSize(t uintptr) (uintptr, error) {
	v, err := g.Attr(t, "__basicsize__")
	if err != nil {
		return 0, err
	}
	defer pyDecRef(v)
	return uintptr(pyLongAsUnsignedLongLong(v)), nil
}

// Borrowed class of an object, read from its header at the measured offset
func pyType(o uintptr) uintptr { return *(*uintptr)(cptr(o + obTypeOffset)) }

// Inserts directories at the front of sys.path in the given order
func (g *GIL) prependSysPath(paths []string) error {
	sysPath := pySysGetObject("path")
	if sysPath == 0 {
		return errors.New("sys.path missing")
	}
	for i := len(paths) - 1; i >= 0; i-- {
		p, err := g.newUnicode(paths[i])
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

// Whether the process is a child created by os.fork from plugin code
func Forked() bool {
	pid := startPid.Load()
	return pid != 0 && int64(os.Getpid()) != pid
}
