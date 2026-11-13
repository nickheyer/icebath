package hostinfo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nameDisplay       = 3
	accountNameLength = 257
	localeNameLength  = 85
)

var (
	procGetUserNameW             = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetUserNameW")
	procGetUserDefaultLocaleName = kernel32.NewProc("GetUserDefaultLocaleName")
)

// Login and display name from the Win32 user name calls, each empty when its call fails
func account() (string, string) {
	var login, full string
	if procGetUserNameW.Find() == nil {
		buf := make([]uint16, accountNameLength)
		size := uint32(len(buf))
		if r, _, _ := procGetUserNameW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
			login = windows.UTF16ToString(buf)
		}
	}
	name := make([]uint16, accountNameLength)
	size := uint32(len(name))
	if windows.GetUserNameEx(nameDisplay, &name[0], &size) == nil {
		full = windows.UTF16ToString(name)
	}
	return login, full
}

// Default locale name of this user such as en-US, empty when the call fails
func hostLocale() string {
	if procGetUserDefaultLocaleName.Find() != nil {
		return ""
	}
	buf := make([]uint16, localeNameLength)
	if r, _, _ := procGetUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&buf[0])), localeNameLength); r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}
