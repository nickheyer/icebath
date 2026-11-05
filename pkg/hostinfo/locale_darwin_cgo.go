//go:build darwin && cgo

package hostinfo

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>

static int hostinfo_preferred_language(char *buf, int size) {
	CFArrayRef languages = CFLocaleCopyPreferredLanguages();
	if (languages == NULL) return 0;
	int ok = 0;
	if (CFArrayGetCount(languages) >= 1) {
		ok = CFStringGetCString((CFStringRef)CFArrayGetValueAtIndex(languages, 0), buf, size, kCFStringEncodingUTF8) ? 1 : 0;
	}
	CFRelease(languages);
	return ok;
}
*/
import "C"

import "unsafe"

// First preferred UI language through CoreFoundation, empty when none
func darwinPreferredLanguage() string {
	buf := (*C.char)(C.malloc(64))
	defer C.free(unsafe.Pointer(buf))
	if C.hostinfo_preferred_language(buf, 64) == 0 {
		return ""
	}
	return C.GoString(buf)
}
