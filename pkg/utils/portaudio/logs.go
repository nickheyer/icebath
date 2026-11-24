package portaudioutil

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

var (
	hooksOnce sync.Once
	// PortAudio's own diagnostics reach the log through its debug print hook, so no call needs its stderr redirected
	ownLogHooked atomic.Bool
	alsaStrerror func(code int32) uintptr
)

// Routes what PortAudio and the libraries under it print into the debug log, each at its source and for the life of the process: PortAudio's debug print function where the build exports it, ALSA's log handler (alsa-lib 1.2.15 and later) and error handler, JACK's error and info functions; a hook whose setter or callback is unavailable is skipped
func installLogHooks(lib *dlutil.Library) {
	hooksOnce.Do(func() {
		ownLogHooked.Store(hook(lib, "PaUtil_SetDebugPrintFunction", portaudioMessage))
		if addr, err := lib.Symbol("snd_strerror"); err == nil {
			dlutil.RegisterFunc(&alsaStrerror, addr)
		}
		hook(lib, "snd_lib_error_set_handler", alsaError)
		hook(lib, "snd_lib_log_set_handler", alsaLog)
		hook(lib, "jack_set_error_function", jackMessage)
		hook(lib, "jack_set_info_function", jackMessage)
	})
}

// Hands a Go function as the handler to a setter the library or one of its dependencies exports, false when either is unavailable
func hook(lib *dlutil.Library, setter string, handler any) bool {
	addr, err := lib.Symbol(setter)
	if err != nil {
		return false
	}
	cb, err := dlutil.NewCallback(handler)
	if err != nil {
		return false
	}
	var set func(handler uintptr)
	dlutil.RegisterFunc(&set, addr)
	set(cb)
	return true
}

// PaUtilLogCallback: one line of PortAudio's own diagnostics. Every handler returns a uintptr the C caller ignores, because Windows makes callbacks only of functions with one pointer sized result
func portaudioMessage(text uintptr) uintptr {
	logNative("portaudio", dlutil.GoString(text))
	return 0
}

// JACK error and info callback: one message
func jackMessage(text uintptr) uintptr {
	logNative("jack", dlutil.GoString(text))
	return 0
}

// snd_lib_log_handler_t: priority, interface, file, line, function, error code, format and its va_list
func alsaLog(_, _, file, line, function, code, format, args uintptr) uintptr {
	logALSA(file, line, function, code, vformat(format, args))
	return 0
}

// snd_lib_error_handler_t, a variadic function: the integer and pointer arguments after the format arrive in the argument slots that follow it, in order, on every 64-bit ABI ALSA runs on
func alsaError(file, line, function, code, format, a0, a1, a2, a3, a4, a5 uintptr) uintptr {
	logALSA(file, line, function, code, cformat(dlutil.GoString(format), []uintptr{a0, a1, a2, a3, a4, a5}))
	return 0
}

// One ALSA message in the form ALSA prints it: ALSA lib file:line:(function) text, then the error's own text
func logALSA(file, line, function, code uintptr, text string) {
	msg := "ALSA lib " + dlutil.GoString(file) + ":" + strconv.Itoa(int(int32(line))) + ":(" + dlutil.GoString(function) + ") " + text
	if errno := int32(code); errno != 0 && alsaStrerror != nil {
		msg += ": " + dlutil.GoString(alsaStrerror(errno))
	}
	logNative("alsa", msg)
}

func logNative(source, text string) {
	if text = strings.TrimRight(text, "\r\n"); text != "" {
		pkgLog().Debug("native audio library message", "source", source, "text", text)
	}
}

// The C library's vsnprintf, nil where it cannot be found
var vsnprintf = sync.OnceValue(func() func(buf *byte, size uintptr, format, args uintptr) int32 {
	addr, err := dlutil.Lookup("vsnprintf")
	if err != nil {
		return nil
	}
	var fn func(buf *byte, size uintptr, format, args uintptr) int32
	dlutil.RegisterFunc(&fn, addr)
	return fn
})

// A C format rendered with its va_list by vsnprintf, at most 1 KiB of it; the bare format where vsnprintf is missing
func vformat(format, args uintptr) string {
	fn := vsnprintf()
	if fn == nil || format == 0 {
		return dlutil.GoString(format)
	}
	var buf [1024]byte
	n := fn(&buf[0], uintptr(len(buf)), format, args)
	switch {
	case n < 0:
		return dlutil.GoString(format)
	case int(n) >= len(buf):
		return string(buf[:len(buf)-1]) + "..."
	}
	return string(buf[:n])
}

// A C format rendered with integer, character, pointer and string arguments taken from slots in order; from the first conversion whose argument is not in an integer slot (floating point) or when the slots run out, the rest of the format is kept as written
func cformat(format string, slots []uintptr) string {
	var b strings.Builder
	take := func() (uintptr, bool) {
		if len(slots) == 0 {
			return 0, false
		}
		v := slots[0]
		slots = slots[1:]
		return v, true
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("-+ #0'", format[j]) >= 0 {
			j++
		}
		if j < len(format) && format[j] == '*' {
			if _, ok := take(); !ok {
				b.WriteString(format[i:])
				return b.String()
			}
			j++
		}
		for j < len(format) && format[j] >= '0' && format[j] <= '9' {
			j++
		}
		precision := -1
		if j < len(format) && format[j] == '.' {
			j++
			precision = 0
			if j < len(format) && format[j] == '*' {
				v, ok := take()
				if !ok {
					b.WriteString(format[i:])
					return b.String()
				}
				precision = int(int32(v))
				j++
			}
			for j < len(format) && format[j] >= '0' && format[j] <= '9' {
				precision = precision*10 + int(format[j]-'0')
				j++
			}
		}
		size := 0
		for j < len(format) && strings.IndexByte("hlqjzt", format[j]) >= 0 {
			switch format[j] {
			case 'h':
				size--
			default:
				size = 1
			}
			j++
		}
		if j >= len(format) {
			b.WriteString(format[i:])
			return b.String()
		}
		conv := format[j]
		if conv == '%' {
			b.WriteByte('%')
			i = j
			continue
		}
		if strings.IndexByte("diuxXocps", conv) < 0 {
			b.WriteString(format[i:])
			return b.String()
		}
		v, ok := take()
		if !ok {
			b.WriteString(format[i:])
			return b.String()
		}
		switch conv {
		case 'd', 'i':
			b.WriteString(strconv.FormatInt(signedSlot(v, size), 10))
		case 'u':
			b.WriteString(strconv.FormatUint(unsignedSlot(v, size), 10))
		case 'x':
			b.WriteString(strconv.FormatUint(unsignedSlot(v, size), 16))
		case 'X':
			b.WriteString(strings.ToUpper(strconv.FormatUint(unsignedSlot(v, size), 16)))
		case 'o':
			b.WriteString(strconv.FormatUint(unsignedSlot(v, size), 8))
		case 'c':
			b.WriteByte(byte(v))
		case 'p':
			b.WriteString("0x" + strconv.FormatUint(uint64(v), 16))
		case 's':
			if v == 0 {
				b.WriteString("(null)")
				break
			}
			if precision >= 0 {
				b.WriteString(boundedString(v, precision))
			} else {
				b.WriteString(dlutil.GoString(v))
			}
		}
		i = j
	}
	return b.String()
}

// A signed C integer argument of a length modifier: hh, h, none, or l and wider
func signedSlot(v uintptr, size int) int64 {
	switch {
	case size <= -2:
		return int64(int8(v))
	case size == -1:
		return int64(int16(v))
	case size == 0:
		return int64(int32(v))
	}
	return int64(int(v))
}

// An unsigned C integer argument of a length modifier: hh, h, none, or l and wider
func unsignedSlot(v uintptr, size int) uint64 {
	switch {
	case size <= -2:
		return uint64(uint8(v))
	case size == -1:
		return uint64(uint16(v))
	case size == 0:
		return uint64(uint32(v))
	}
	return uint64(v)
}

// The C string at p up to its NUL or its first n bytes, whichever ends first: a precision bounds how far printf reads, so the bytes after it need not be a string at all
func boundedString(p uintptr, n int) string {
	base := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	end := 0
	for end < n && *(*byte)(unsafe.Add(base, end)) != 0 {
		end++
	}
	return string(unsafe.Slice((*byte)(base), end))
}
