//go:build unix

package portaudioutil

import (
	"bufio"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

// The C library's stderr stream variable and a stream into the debug log to point it at; the log stream writes into a pipe a goroutine drains and is never closed, so a thread that picked it up during a window can still write to it afterwards
type stderrRoute struct {
	slot  unsafe.Pointer
	log   uintptr
	flush func(file uintptr) int32
}

// Routes are found once; nil where the C library has no writable stderr variable (musl's is const, NetBSD's is an array element) or the stdio functions or a pipe are missing
var route = sync.OnceValue(func() *stderrRoute {
	var slot unsafe.Pointer
	for _, name := range []string{"stderr", "__stderrp"} {
		if addr, err := dlutil.Lookup(name); err == nil {
			slot = *(*unsafe.Pointer)(unsafe.Pointer(&addr))
			break
		}
	}
	if slot == nil || !storeSlot(slot, *(*uintptr)(slot)) {
		return nil
	}
	var (
		fdopen  func(fd int32, mode string) uintptr
		setvbuf func(file, buf uintptr, mode int32, size uintptr) int32
		flush   func(file uintptr) int32
	)
	for _, fn := range []struct {
		fptr any
		name string
	}{{&fdopen, "fdopen"}, {&setvbuf, "setvbuf"}, {&flush, "fflush"}} {
		addr, err := dlutil.Lookup(fn.name)
		if err != nil {
			return nil
		}
		dlutil.RegisterFunc(fn.fptr, addr)
	}
	var fds [2]int
	syscall.ForkLock.RLock()
	err := syscall.Pipe(fds[:])
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil
	}
	file := fdopen(int32(fds[1]), "w")
	if file == 0 {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
		return nil
	}
	setvbuf(file, 0, unbufferedMode(), 0)
	_ = syscall.SetNonblock(fds[0], true)
	go drainStderr(os.NewFile(uintptr(fds[0]), "portaudio stderr"))
	return &stderrRoute{slot: slot, log: file, flush: flush}
})

// _IONBF of the C library
func unbufferedMode() int32 {
	switch runtime.GOOS {
	case "aix", "illumos", "solaris":
		return 4
	}
	return 2
}

// Writes the stream variable, false when the C library keeps it in read-only memory
func storeSlot(slot unsafe.Pointer, v uintptr) (ok bool) {
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	*(*uintptr)(slot) = v
	return true
}

// Logs each line written into the route's pipe
func drainStderr(r *os.File) {
	lines := bufio.NewReader(r)
	for {
		line, err := lines.ReadString('\n')
		logNative("stderr", strings.TrimRight(line, "\r\n"))
		if err != nil {
			return
		}
	}
}

var window struct {
	mu    sync.Mutex
	depth int
	saved uintptr
}

// Runs a PortAudio call that opens or releases host devices with the C library's stderr stream pointed into the debug log, because PortAudio prints its own diagnostics with vfprintf(stderr) and exports no hook for them (PaUtil_SetDebugPrintFunction is not in the dynamic symbol table of the distribution builds). Overlapping calls share one window, the stream is put back the moment the last returns, and Go's own stderr writes never pass through it. Where PortAudio's hook exists, or there is no writable stream variable, the call simply runs
func quietly(call func()) {
	r := route()
	if ownLogHooked.Load() || r == nil {
		call()
		return
	}
	window.mu.Lock()
	if window.depth == 0 {
		window.saved = *(*uintptr)(r.slot)
		storeSlot(r.slot, r.log)
	}
	window.depth++
	window.mu.Unlock()
	defer func() {
		window.mu.Lock()
		if window.depth--; window.depth == 0 {
			storeSlot(r.slot, window.saved)
			r.flush(r.log)
		}
		window.mu.Unlock()
	}()
	call()
}
