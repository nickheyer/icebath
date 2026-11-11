//go:build linux && !386 && !arm && !faketime

package pyplatform

import (
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

const csGNULibcVersion = 2

// C library name and version the libc of this process reports through confstr, false when it reports none
func confstrLibcVer() (string, string, bool) {
	confstr, err := purego.Dlsym(purego.RTLD_DEFAULT, "confstr")
	if err != nil || confstr == 0 {
		return "", "", false
	}
	buf := make([]byte, 255)
	n, _, _ := purego.SyscallN(confstr, csGNULibcVersion, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	runtime.KeepAlive(buf)
	if n == 0 || int(n) > len(buf) {
		return "", "", false
	}
	parts := strings.Fields(string(buf[:n-1]))
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(string(buf[:n-1]), " \t\n\r\v\f"), parts[0]), " \t\n\r\v\f"), true
}
