//go:build windows

package pm

import (
	"errors"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/nickheyer/icebath/pkg/home"
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

// Default root a new console resolves from the user's own environment block, false when unreadable
func newConsoleRoot() (string, bool) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token); err != nil {
		return "", false
	}
	defer token.Close()
	block, err := token.Environ(false)
	if err != nil {
		return "", false
	}
	environ := map[string]string{}
	for _, entry := range block {
		name, value, _ := strings.Cut(entry, "=")
		if name != "" {
			environ[strings.ToUpper(name)] = value
		}
	}
	return home.RootForEnvironment(environ), true
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
