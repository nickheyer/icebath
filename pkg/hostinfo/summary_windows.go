package hostinfo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetUserNameW         = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetUserNameW")
	procGetUserNameExW       = windows.NewLazySystemDLL("secur32.dll").NewProc("GetUserNameExW")
	procGetUserDefaultLocale = modKernel32.NewProc("GetUserDefaultLocaleName")
)

// Login and display name from the Win32 account APIs
func account() (string, string) {
	var login, full string
	loginBuf := make([]uint16, 257)
	loginLen := uint32(len(loginBuf))
	if r, _, _ := procGetUserNameW.Call(uintptr(unsafe.Pointer(&loginBuf[0])), uintptr(unsafe.Pointer(&loginLen))); r != 0 {
		login = windows.UTF16ToString(loginBuf)
	}
	nameBuf := make([]uint16, 257)
	nameLen := uint32(len(nameBuf))
	if r, _, _ := procGetUserNameExW.Call(3, uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(unsafe.Pointer(&nameLen))); r != 0 {
		full = windows.UTF16ToString(nameBuf)
	}
	return login, full
}

// The user default locale name, empty when the call fails
func osLocale() string {
	buf := make([]uint16, 85)
	if r, _, _ := procGetUserDefaultLocale.Call(uintptr(unsafe.Pointer(&buf[0])), 85); r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// Windows version as major.minor.build from the kernel
func osRelease() string {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	return itoa(int(major)) + "." + itoa(int(minor)) + "." + itoa(int(build))
}

// Decimal text of a non negative integer
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
