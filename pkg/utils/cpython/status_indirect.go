//go:build ((darwin && amd64) || (linux && (cgo || !android) && (amd64 || loong64 || ppc64le || riscv64 || (cgo && s390x))) || (windows && amd64)) && !faketime

package cpythonutil

import "unsafe"

// Every ABI here returns the 32 byte PyStatus through memory whose address the caller passes as a hidden first argument (RDI on System V amd64, RCX on Windows x64, a0 on riscv64 and loong64, r3 on ppc64le, r2 on s390x), so the buffer is an explicit first argument and purego needs no struct return support
var (
	pyPreInitialize        func(ret *pyStatus, pre uintptr) uintptr
	pyConfigSetString      func(ret *pyStatus, cfg, field uintptr, s unsafe.Pointer) uintptr
	pyConfigSetArgv        func(ret *pyStatus, cfg uintptr, argc int, argv unsafe.Pointer) uintptr
	pyInitializeFromConfig func(ret *pyStatus, cfg uintptr) uintptr
)

// Resolves the PyStatus returning entry points with the result buffer as an explicit first argument
func bindStatus(lookup func(string) (uintptr, error)) error {
	ptrs := []any{&pyPreInitialize, &pyConfigSetString, &pyConfigSetArgv, &pyInitializeFromConfig}
	for i, name := range statusSymbolNames {
		addr, err := lookup(name)
		if err != nil || addr == 0 {
			return &SymbolError{Name: name, Err: err}
		}
		registerFunc(ptrs[i], addr)
	}
	return nil
}

func statusPreInitialize(pre uintptr) pyStatus {
	var st pyStatus
	pyPreInitialize(&st, pre)
	return st
}

func statusConfigSetString(cfg, field uintptr, s unsafe.Pointer) pyStatus {
	var st pyStatus
	pyConfigSetString(&st, cfg, field, s)
	return st
}

func statusConfigSetArgv(cfg uintptr, argc int, argv unsafe.Pointer) pyStatus {
	var st pyStatus
	pyConfigSetArgv(&st, cfg, argc, argv)
	return st
}

func statusInitializeFromConfig(cfg uintptr) pyStatus {
	var st pyStatus
	pyInitializeFromConfig(&st, cfg)
	return st
}
