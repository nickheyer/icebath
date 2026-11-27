//go:build unix

package portaudio

import (
	"bufio"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/nickheyer/icebath/internal/speech/native"
)

// The C library's stderr variable and a stream into the debug log to point it at, its pipe drained by a goroutine and never closed so a
// thread that picked it up in a window can still write to it after
type stderrRoute struct {
	slot  unsafe.Pointer
	log   uintptr
	flush func(file uintptr) int32
}

// Found once; nil where the C library has no writable stderr variable (musl's is const, NetBSD's an array element) or stdio or a pipe is
// missing
var route = sync.OnceValue(func() *stderrRoute {
	var slot unsafe.Pointer
	for _, name := range []string{"stderr", "__stderrp"} {
		if addr, err := native.Lookup(name); err == nil {
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
		addr, err := native.Lookup(fn.name)
		if err != nil {
			return nil
		}
		native.RegisterFunc(fn.fptr, addr)
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
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil
	}
	setvbuf(file, 0, unbuffered(), 0)
	syscall.SetNonblock(fds[0], true)
	go drain(os.NewFile(uintptr(fds[0]), "portaudio stderr"))
	return &stderrRoute{slot: slot, log: file, flush: flush}
})

// _IONBF of the C library
func unbuffered() int32 {
	switch runtime.GOOS {
	case "aix", "illumos", "solaris":
		return 4
	}
	return 2
}

// Writes the stream variable, false when it lives in read-only memory
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

func drain(r *os.File) {
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

// Runs a call that opens or releases devices with the C library's stderr pointed into the debug log, since PortAudio prints with
// vfprintf(stderr) and the distribution builds export no hook; overlapping calls share one window, the stream put back when the last
// returns. Where the hook exists or the variable can't be written the call simply runs
func quietly(call func()) {
	r := route()
	if ownHooked.Load() || r == nil {
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
