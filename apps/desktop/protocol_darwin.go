//go:build darwin

package main

/*
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <stdlib.h>
#include <CoreServices/CoreServices.h>

static int icebath_set_handler(const char *scheme, const char *bundle) {
	CFStringRef s = CFStringCreateWithCString(NULL, scheme, kCFStringEncodingUTF8);
	CFStringRef b = CFStringCreateWithCString(NULL, bundle, kCFStringEncodingUTF8);
	OSStatus st = LSSetDefaultHandlerForURLScheme(s, b);
	CFRelease(s);
	CFRelease(b);
	return (int)st;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type protocolRegistrar struct{}

// SetAsDefaultProtocolClient makes the app bundle the scheme handler through Launch Services
func (protocolRegistrar) SetAsDefaultProtocolClient(protocol, execPath string, args []string) error {
	scheme := C.CString(protocol)
	defer C.free(unsafe.Pointer(scheme))
	bundle := C.CString(bundleID)
	defer C.free(unsafe.Pointer(bundle))
	if st := C.icebath_set_handler(scheme, bundle); st != 0 {
		return fmt.Errorf("LSSetDefaultHandlerForURLScheme returned %d", int(st))
	}
	return nil
}
