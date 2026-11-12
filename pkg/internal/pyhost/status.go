package pyhost

import (
	"fmt"
	"unsafe"
)

// PyStatus as laid out by CPython, an enum, two C strings and an exit code
type pyStatus struct {
	Type     int32
	Pad0     int32
	Func     uintptr
	ErrMsg   uintptr
	ExitCode int32
	Pad1     int32
}

var statusSymbolNames = []string{"Py_PreInitialize", "PyConfig_SetString", "PyConfig_SetArgv", "Py_InitializeFromConfig"}

// Error carrying the failing C function and message, nil for an OK status
func (s pyStatus) err(op string) error {
	switch s.Type {
	case 0:
		return nil
	case 2:
		return fmt.Errorf("%s: interpreter requested exit with code %d", op, s.ExitCode)
	}
	return fmt.Errorf("%s: %s: %s", op, cString(s.Func), cString(s.ErrMsg))
}

// Copies a NUL terminated C string into Go memory
func cString(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(cptr(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(cptr(p)), n))
}

// Pointer to C memory at addr, the form vet accepts for addresses that came from the C side
func cptr(addr uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&addr)) }
