//go:build windows

package selfupdate

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetMachineTypeAttributes = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetMachineTypeAttributes")

// PE machines this host runs in user mode per GetMachineTypeAttributes, an error before Windows 11
func windowsUserRunnablePEMachines() (map[uint16]bool, error) {
	if err := procGetMachineTypeAttributes.Find(); err != nil {
		return nil, err
	}
	runnable := map[uint16]bool{}
	for _, machine := range []uint16{peMachineARM64, peMachineAMD64, peMachineI386} {
		var attributes int32
		hr, _, _ := procGetMachineTypeAttributes.Call(uintptr(machine), uintptr(unsafe.Pointer(&attributes)))
		if hr != 0 {
			continue
		}
		if attributes&machineAttributeUserEnabled != 0 {
			runnable[machine] = true
		}
	}
	return runnable, nil
}
