//go:build !windows

package selfupdate

import "errors"

// GetMachineTypeAttributes exists only on Windows
func windowsUserRunnablePEMachines() (map[uint16]bool, error) {
	return nil, errors.New("GetMachineTypeAttributes is a Windows API")
}
