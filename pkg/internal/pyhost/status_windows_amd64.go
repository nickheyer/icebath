//go:build windows && amd64

package pyhost

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The Windows x64 convention passes the hidden PyStatus result pointer as the first argument in RCX
var (
	pyPreInitialize       func(ret *pyStatus, pre uintptr) uintptr
	pyConfigSetString     func(ret *pyStatus, cfg, field uintptr, s unsafe.Pointer) uintptr
	pyConfigSetArgv       func(ret *pyStatus, cfg uintptr, argc int, argv unsafe.Pointer) uintptr
	pyInitializeFromConfg func(ret *pyStatus, cfg uintptr) uintptr
)

func bindStatus(lookup func(string) (uintptr, error)) error {
	ptrs := []any{&pyPreInitialize, &pyConfigSetString, &pyConfigSetArgv, &pyInitializeFromConfg}
	for i, name := range statusSymbolNames {
		addr, err := lookup(name)
		if err != nil || addr == 0 {
			return fmt.Errorf("libpython lacks %s: %v", name, err)
		}
		purego.RegisterFunc(ptrs[i], addr)
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
	pyInitializeFromConfg(&st, cfg)
	return st
}
