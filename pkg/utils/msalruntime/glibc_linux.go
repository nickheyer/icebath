//go:build linux && (amd64 || arm64) && !faketime

package msalruntime

import (
	"strconv"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Major and minor version of the GNU C library this process runs on, false on another C library
func glibcVersion() (int, int, bool) {
	lib, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, 0, false
	}
	fn, err := purego.Dlsym(lib, "gnu_get_libc_version")
	if err != nil {
		return 0, 0, false
	}
	text, _, _ := purego.SyscallN(fn)
	if text == 0 {
		return 0, 0, false
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&text))
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	left, right, _ := strings.Cut(string(unsafe.Slice((*byte)(p), n)), ".")
	major, errMajor := strconv.Atoi(left)
	minor, errMinor := strconv.Atoi(strings.SplitN(right, ".", 2)[0])
	return major, minor, errMajor == nil && errMinor == nil
}
