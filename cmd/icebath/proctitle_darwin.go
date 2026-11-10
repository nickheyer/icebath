package main

import (
	"unsafe"

	"github.com/ebitengine/purego"
)

// Names the calling thread through pthread_setname_np of libc the way hermes does on macOS, every failure ignored
func setProcessTitleOS(title string) {
	defer func() { _ = recover() }()
	libc, err := purego.Dlopen("libc.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return
	}
	setName, err := purego.Dlsym(libc, "pthread_setname_np")
	if err != nil || setName == 0 {
		return
	}
	name := append([]byte(title), 0)
	purego.SyscallN(setName, uintptr(unsafe.Pointer(&name[0])))
}
