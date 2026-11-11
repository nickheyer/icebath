//go:build !windows

package selfupdate

import "errors"

// The user PATH lives in the Windows registry
func readUserPathRaw() ([]string, uint32, error) {
	return nil, 0, errors.New("the user PATH registry value exists only on Windows")
}

// The user PATH lives in the Windows registry
func writeUserPathRaw([]string, uint32) error {
	return errors.New("the user PATH registry value exists only on Windows")
}

// Percent variables expand only on Windows, the posix expandvars leaves them
func windowsExpandVars(entry string) string { return entry }
