//go:build darwin

package system

import (
	"sync"

	"github.com/ebitengine/purego"
)

const (
	kCFStringEncodingUTF8 = 0x08000100
	kIOPMAssertionLevelOn = 255
)

var iokit = sync.OnceValues(func() (*iokitFuncs, error) {
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	io, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	f := &iokitFuncs{}
	purego.RegisterLibFunc(&f.cfStringCreate, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&f.cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&f.assertionCreate, io, "IOPMAssertionCreateWithName")
	purego.RegisterLibFunc(&f.assertionRelease, io, "IOPMAssertionRelease")
	return f, nil
})

type iokitFuncs struct {
	cfStringCreate   func(alloc uintptr, s string, encoding uint32) uintptr
	cfRelease        func(ref uintptr)
	assertionCreate  func(kind uintptr, level uint32, name uintptr, id *uint32) int32
	assertionRelease func(id uint32) int32
}

func (f *iokitFuncs) cfString(s string) uintptr { return f.cfStringCreate(0, s, kCFStringEncodingUTF8) }

type iopmHold struct {
	once sync.Once
	f    *iokitFuncs
	id   uint32
}

func (h *iopmHold) Release() { h.once.Do(func() { h.f.assertionRelease(h.id) }) }

// Takes an IOKit power assertion, idle system sleep for app suspension and idle display sleep otherwise
func platformInhibit(kind KeepAwakeType, reason string) Hold {
	f, err := iokit()
	if err != nil {
		return nil
	}
	assertion := "PreventUserIdleSystemSleep"
	if kind == PreventDisplaySleep {
		assertion = "PreventUserIdleDisplaySleep"
	}
	kindRef := f.cfString(assertion)
	nameRef := f.cfString(reason)
	defer f.cfRelease(kindRef)
	defer f.cfRelease(nameRef)
	var id uint32
	if f.assertionCreate(kindRef, kIOPMAssertionLevelOn, nameRef, &id) != 0 {
		return nil
	}
	return &iopmHold{f: f, id: id}
}
