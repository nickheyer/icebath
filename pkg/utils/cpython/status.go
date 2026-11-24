package cpythonutil

import "unsafe"

// PyStatus as CPython lays it out on 64-bit systems, an enum, two C strings and an exit code, filled by the C function through the buffer address it is handed
type pyStatus struct {
	Type     int32
	Pad0     int32
	Func     uintptr
	ErrMsg   uintptr
	ExitCode int32
	Pad1     int32
}

var statusSymbolNames = []string{"Py_PreInitialize", "PyConfig_SetString", "PyConfig_SetArgv", "Py_InitializeFromConfig"}

// InitError carrying the failing C function and message or the requested exit, nil for an OK status
func (s pyStatus) err(op string) error {
	switch s.Type {
	case 0:
		return nil
	case 2:
		return &InitError{Op: op, Exit: true, ExitCode: int(s.ExitCode)}
	}
	return &InitError{Op: op, Func: cString(s.Func), Message: cString(s.ErrMsg)}
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
