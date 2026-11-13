package hostinfo

import (
	"encoding/binary"
	"runtime"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
)

const (
	coreFoundationPath   = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	ioKitPath            = "/System/Library/Frameworks/IOKit.framework/IOKit"
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt64Type   = 4
)

var (
	coreFoundationSymbols = []string{"CFRelease", "CFGetTypeID", "CFDataGetTypeID", "CFDataGetLength", "CFDataGetBytes",
		"CFStringGetTypeID", "CFStringCreateWithCString", "CFStringGetCString", "CFStringCompare", "CFStringGetLength",
		"CFStringGetMaximumSizeForEncoding", "CFNumberGetTypeID",
		"CFNumberGetValue", "CFArrayGetCount", "CFArrayGetValueAtIndex", "CFDictionaryGetValue", "CFLocaleCopyPreferredLanguages"}
	ioKitSymbols = []string{"IOServiceMatching", "IOServiceGetMatchingServices", "IOIteratorNext", "IORegistryEntryCreateCFProperty",
		"IOObjectRelease", "IOPSCopyPowerSourcesInfo", "IOPSCopyPowerSourcesList", "IOPSGetPowerSourceDescription"}
)

// CoreFoundation and IOKit symbols every probe calls, resolved once for the process
type frameworks struct {
	symbols map[string]uintptr
}

// Frameworks opened on first use and kept loaded for the life of the process
var loadFrameworks = sync.OnceValues(openFrameworks)

// Opens CoreFoundation and IOKit and resolves every symbol a probe calls
func openFrameworks() (*frameworks, error) {
	f := &frameworks{symbols: map[string]uintptr{}}
	for _, lib := range []struct {
		path    string
		symbols []string
	}{{coreFoundationPath, coreFoundationSymbols}, {ioKitPath, ioKitSymbols}} {
		library, err := dl.Open(lib.path, dl.Global)
		if err != nil {
			return nil, err
		}
		for _, name := range lib.symbols {
			addr, err := library.Symbol(name)
			if err != nil {
				return nil, err
			}
			f.symbols[name] = addr
		}
	}
	return f, nil
}

//go:uintptrescapes
func (f *frameworks) call(name string, args ...uintptr) uintptr {
	r, _ := dl.Call(f.symbols[name], args...)
	return r
}

// CFString of s that the caller releases
func (f *frameworks) cfString(s string) uintptr {
	text := append([]byte(s), 0)
	ref := f.call("CFStringCreateWithCString", 0, uintptr(unsafe.Pointer(&text[0])), cfStringEncodingUTF8)
	runtime.KeepAlive(text)
	return ref
}

// Releases a CoreFoundation object unless it is null
func (f *frameworks) release(ref uintptr) {
	if ref != 0 {
		f.call("CFRelease", ref)
	}
}

// Reports whether ref is a non null object of the type the named type function returns
func (f *frameworks) isType(ref uintptr, typeFunc string) bool {
	return ref != 0 && f.call("CFGetTypeID", ref) == f.call(typeFunc)
}

// Bytes of a CFData, false for null or another type
func (f *frameworks) data(ref uintptr) ([]byte, bool) {
	if !f.isType(ref, "CFDataGetTypeID") {
		return nil, false
	}
	n := int(f.call("CFDataGetLength", ref))
	if n <= 0 {
		return nil, n == 0
	}
	buf := make([]byte, n)
	f.call("CFDataGetBytes", ref, 0, uintptr(n), uintptr(unsafe.Pointer(&buf[0])))
	return buf, true
}

// Value of a CFNumber as a 64 bit integer, false for null or another type
func (f *frameworks) number(ref uintptr) (int64, bool) {
	if !f.isType(ref, "CFNumberGetTypeID") {
		return 0, false
	}
	var value int64
	ok := f.call("CFNumberGetValue", ref, cfNumberSInt64Type, uintptr(unsafe.Pointer(&value)))&0xFF != 0
	return value, ok
}

// UTF-8 contents of a CFString, false for null or another type
func (f *frameworks) text(ref uintptr) (string, bool) {
	if !f.isType(ref, "CFStringGetTypeID") {
		return "", false
	}
	length := f.call("CFStringGetLength", ref)
	buf := make([]byte, int(f.call("CFStringGetMaximumSizeForEncoding", length, cfStringEncodingUTF8))+1)
	if f.call("CFStringGetCString", ref, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), cfStringEncodingUTF8)&0xFF == 0 {
		return "", false
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), true
		}
	}
	return string(buf), true
}

// Unsigned little endian integer of up to eight bytes, false when empty
func littleEndian(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var padded [8]byte
	copy(padded[:], b)
	return binary.LittleEndian.Uint64(padded[:]), true
}
