//go:build linux && !386 && !arm && (cgo || (!android && (amd64 || arm64 || loong64 || ppc64le || riscv64))) && !faketime

package hostutil

import (
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// confstr name of the GNU C library version string
const csGNULibcVersion = 2

// Name and version the C library loaded into this process reports through confstr, false when none is loaded or it does not say
func confstrLibc() (string, string, bool) {
	confstr, err := purego.Dlsym(purego.RTLD_DEFAULT, "confstr")
	if err != nil || confstr == 0 {
		return "", "", false
	}
	buf := make([]byte, 256)
	n, _, _ := purego.SyscallN(confstr, csGNULibcVersion, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	runtime.KeepAlive(buf)
	if n == 0 || n > uintptr(len(buf)) {
		return "", "", false
	}
	name, version, ok := strings.Cut(strings.TrimSpace(string(buf[:n-1])), " ")
	if !ok || name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(version), true
}
