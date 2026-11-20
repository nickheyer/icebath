//go:build !windows

package pm

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Reports execute permission for the current user
func executable(path string) bool { return unix.Access(path, unix.X_OK) == nil }

// POSIX paths compare byte for byte
func pathNormcase(path string) string { return path }

// No console environment block exists outside Windows
func consoleEnvironment() (map[string]string, bool) { return nil, false }

// The User PATH lives in the Windows registry
func registerWindowsUserPath(string) (string, error) {
	return "", errors.New("the User PATH lives in the Windows registry")
}
