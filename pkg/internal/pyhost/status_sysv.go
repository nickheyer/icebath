//go:build (darwin || linux) && !arm64

package pyhost

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// System V returns the 32 byte PyStatus through memory that purego allocates and passes in RDI
var (
	pyPreInitialize       func(pre uintptr) pyStatus
	pyConfigSetString     func(cfg, field uintptr, s unsafe.Pointer) pyStatus
	pyConfigSetArgv       func(cfg uintptr, argc int, argv unsafe.Pointer) pyStatus
	pyInitializeFromConfg func(cfg uintptr) pyStatus
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

func statusPreInitialize(pre uintptr) pyStatus { return pyPreInitialize(pre) }
func statusConfigSetString(cfg, field uintptr, s unsafe.Pointer) pyStatus {
	return pyConfigSetString(cfg, field, s)
}
func statusConfigSetArgv(cfg uintptr, argc int, argv unsafe.Pointer) pyStatus {
	return pyConfigSetArgv(cfg, argc, argv)
}
func statusInitializeFromConfig(cfg uintptr) pyStatus { return pyInitializeFromConfg(cfg) }
