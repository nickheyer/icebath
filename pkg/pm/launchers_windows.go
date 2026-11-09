//go:build windows

package pm

import (
	"errors"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows grants execute to any existing file
func executable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Lowercased backslash form Windows path comparison uses
func pathNormcase(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "/", `\`))
}

// Environment of a new console of this user from CreateEnvironmentBlock, false when unreadable
func consoleEnvironment() (map[string]string, bool) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token); err != nil {
		return nil, false
	}
	defer token.Close()
	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, token, false); err != nil {
		return nil, false
	}
	defer windows.DestroyEnvironmentBlock(block)
	return readEnvironmentBlock(environmentBlockUnits(block)), true
}

// Copies a double NUL terminated UTF-16 block into a slice holding its final empty entry
func environmentBlockUnits(block *uint16) []uint16 {
	var out []uint16
	for p := unsafe.Pointer(block); ; p = unsafe.Add(p, 2) {
		unit := *(*uint16)(p)
		out = append(out, unit)
		if unit == 0 && (len(out) == 1 || out[len(out)-2] == 0) {
			return out
		}
	}
}

// Puts entry first on the User PATH keeping the stored value kind, reporting present or added
func registerWindowsUserPath(entry string) (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()
	current, kind, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		current, kind = "", registry.EXPAND_SZ
	} else if err != nil {
		return "", err
	}
	merged, changed := mergeUserPath(current, entry)
	if !changed {
		return "present", nil
	}
	if kind == registry.SZ {
		err = key.SetStringValue("Path", merged)
	} else {
		err = key.SetExpandStringValue("Path", merged)
	}
	if err != nil {
		return "", err
	}
	broadcastEnvironmentChange()
	return "added", nil
}

// Tells running top level windows the environment changed
func broadcastEnvironmentChange() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	send := user32.NewProc("SendMessageTimeoutW")
	name, err := windows.UTF16PtrFromString("Environment")
	if err != nil || send.Find() != nil {
		return
	}
	var result uintptr
	send.Call(0xFFFF, 0x1A, 0, uintptr(unsafe.Pointer(name)), 0x0002, 5000, uintptr(unsafe.Pointer(&result)))
}
