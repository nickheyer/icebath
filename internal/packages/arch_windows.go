package packages

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var isWow64Process2 = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsWow64Process2")

// Whether the machine is arm64 under the x64 emulation this process runs in
func emulatedOnArm64() bool {
	if isWow64Process2.Find() == nil {
		var process, native uint16
		if r, _, _ := isWow64Process2.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native))); r != 0 && native != 0 {
			return native == 0xAA64
		}
	}
	arch := os.Getenv("PROCESSOR_ARCHITEW6432")
	if arch == "" {
		arch = os.Getenv("PROCESSOR_ARCHITECTURE")
	}
	return strings.EqualFold(arch, "ARM64")
}
