//go:build darwin

package system

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

type darwinFrameworks struct {
	appKit, cf, iokit, cg uintptr
	runLoopGetMain        func() uintptr
	runLoopAddSource      func(rl, source, mode uintptr)
	stringGetCString      func(s uintptr, buf *byte, size int, encoding uint32) bool
	cfRelease             func(ref uintptr)
	psNotifySource        func(callback, context uintptr) uintptr
	psCopyInfo            func() uintptr
	psProvidingType       func(blob uintptr) uintptr
	secondsSinceLastEvent func(state int32, kind uint32) float64
}

var darwinLibs = sync.OnceValues(func() (*darwinFrameworks, error) {
	f := &darwinFrameworks{}
	var err error
	open := func(path string) uintptr {
		if err != nil {
			return 0
		}
		var h uintptr
		h, err = purego.Dlopen(path, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		return h
	}
	open("/System/Library/Frameworks/Foundation.framework/Foundation")
	f.appKit = open("/System/Library/Frameworks/AppKit.framework/AppKit")
	f.cf = open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
	f.iokit = open("/System/Library/Frameworks/IOKit.framework/IOKit")
	f.cg = open("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
	if err != nil {
		return nil, err
	}
	purego.RegisterLibFunc(&f.runLoopGetMain, f.cf, "CFRunLoopGetMain")
	purego.RegisterLibFunc(&f.runLoopAddSource, f.cf, "CFRunLoopAddSource")
	purego.RegisterLibFunc(&f.stringGetCString, f.cf, "CFStringGetCString")
	purego.RegisterLibFunc(&f.cfRelease, f.cf, "CFRelease")
	purego.RegisterLibFunc(&f.psNotifySource, f.iokit, "IOPSNotificationCreateRunLoopSource")
	purego.RegisterLibFunc(&f.psCopyInfo, f.iokit, "IOPSCopyPowerSourcesInfo")
	purego.RegisterLibFunc(&f.psProvidingType, f.iokit, "IOPSGetProvidingPowerSourceType")
	purego.RegisterLibFunc(&f.secondsSinceLastEvent, f.cg, "CGEventSourceSecondsSinceLastEventType")
	return f, nil
})

// Value of an exported Objective-C or CoreFoundation constant object
func darwinConstant(lib uintptr, name string) uintptr {
	p, err := purego.Dlsym(lib, name)
	if err != nil || p == 0 {
		return 0
	}
	return *(*uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&p)))
}

func objcString(s string) objc.ID {
	b := append([]byte(s), 0)
	return objc.ID(objc.GetClass("NSString")).Send(objc.RegisterName("stringWithUTF8String:"), unsafe.Pointer(&b[0]))
}

func (f *darwinFrameworks) cfGoString(ref uintptr) string {
	if ref == 0 {
		return ""
	}
	buf := make([]byte, 256)
	if !f.stringGetCString(ref, &buf[0], len(buf), kCFStringEncodingUTF8) {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// DarwinPower watches NSWorkspace sleep and wake, the screen unlock notification and IOKit power sources
type DarwinPower struct {
	mu      sync.Mutex
	f       *darwinFrameworks
	battery bool
	blocks  []objc.Block
}

// Power monitor over AppKit and IOKit
func NewPowerMonitor() *DarwinPower { return &DarwinPower{} }

func (p *DarwinPower) observe(center objc.ID, name objc.ID, f func()) {
	block := objc.NewBlock(func(objc.Block, objc.ID) { f() })
	p.blocks = append(p.blocks, block)
	queue := objc.ID(objc.GetClass("NSOperationQueue")).Send(objc.RegisterName("mainQueue"))
	center.Send(objc.RegisterName("addObserverForName:object:queue:usingBlock:"), name, objc.ID(0), queue, block)
}

// Registers the workspace, distributed and power source observers on the main run loop
func (p *DarwinPower) Start(ev PowerEvents) error {
	f, err := darwinLibs()
	if err != nil {
		return err
	}
	p.f = f
	willSleep := darwinConstant(f.appKit, "NSWorkspaceWillSleepNotification")
	didWake := darwinConstant(f.appKit, "NSWorkspaceDidWakeNotification")
	if willSleep == 0 || didWake == 0 {
		return errors.New("NSWorkspace sleep notifications are unavailable")
	}
	workspace := objc.ID(objc.GetClass("NSWorkspace")).Send(objc.RegisterName("sharedWorkspace"))
	center := workspace.Send(objc.RegisterName("notificationCenter"))
	p.observe(center, objc.ID(willSleep), ev.Suspend)
	p.observe(center, objc.ID(didWake), ev.Resume)
	distributed := objc.ID(objc.GetClass("NSDistributedNotificationCenter")).Send(objc.RegisterName("defaultCenter"))
	p.observe(distributed, objcString("com.apple.screenIsUnlocked"), ev.UnlockScreen)
	p.battery = p.readBattery()
	callback := purego.NewCallback(func(uintptr) uintptr {
		now := p.readBattery()
		p.mu.Lock()
		changed := now != p.battery
		p.battery = now
		p.mu.Unlock()
		if changed && now {
			ev.OnBattery()
		} else if changed {
			ev.OnAC()
		}
		return 0
	})
	if source := f.psNotifySource(callback, 0); source != 0 {
		f.runLoopAddSource(f.runLoopGetMain(), source, darwinConstant(f.cf, "kCFRunLoopDefaultMode"))
	}
	return nil
}

func (p *DarwinPower) readBattery() bool {
	blob := p.f.psCopyInfo()
	if blob == 0 {
		return false
	}
	defer p.f.cfRelease(blob)
	return p.f.cfGoString(p.f.psProvidingType(blob)) == "Battery Power"
}

// True while the providing power source is the battery
func (p *DarwinPower) IsOnBatteryPower() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.battery
}

// Seconds since the last input event of any kind in the combined session
func (p *DarwinPower) SystemIdleTime() int {
	if p.f == nil {
		return 0
	}
	return int(p.f.secondsSinceLastEvent(0, 0xFFFFFFFF))
}
