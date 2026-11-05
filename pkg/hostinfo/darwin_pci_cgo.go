//go:build darwin && cgo

package hostinfo

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>

static int hostinfo_data_int(CFTypeRef ref, unsigned long long *out) {
	if (ref == NULL) return 0;
	int ok = 0;
	if (CFGetTypeID(ref) == CFDataGetTypeID()) {
		CFIndex size = CFDataGetLength((CFDataRef)ref);
		if (size > 0) {
			const UInt8 *bytes = CFDataGetBytePtr((CFDataRef)ref);
			unsigned long long value = 0;
			for (CFIndex i = size - 1; i >= 0; i--) value = (value << 8) | bytes[i];
			*out = value;
			ok = 1;
		}
	}
	CFRelease(ref);
	return ok;
}

// Fills vendors with the hex vendor id of every display controller, -1 entries when unreadable, returns count or -1
static int hostinfo_display_vendors(long long *vendors, int max) {
	io_iterator_t iterator = 0;
	if (IOServiceGetMatchingServices(kIOMasterPortDefault, IOServiceMatching("IOPCIDevice"), &iterator) != KERN_SUCCESS) return -1;
	CFStringRef classKey = CFStringCreateWithCString(NULL, "class-code", kCFStringEncodingUTF8);
	CFStringRef vendorKey = CFStringCreateWithCString(NULL, "vendor-id", kCFStringEncodingUTF8);
	int count = 0;
	for (int i = 0; i < max; i++) {
		io_registry_entry_t entry = IOIteratorNext(iterator);
		if (!entry) break;
		unsigned long long classCode = 0, vendor = 0;
		int hasClass = hostinfo_data_int(IORegistryEntryCreateCFProperty(entry, classKey, NULL, 0), &classCode);
		int hasVendor = hostinfo_data_int(IORegistryEntryCreateCFProperty(entry, vendorKey, NULL, 0), &vendor);
		IOObjectRelease(entry);
		if (hasClass && (classCode >> 16) == 0x03) {
			vendors[count++] = hasVendor ? (long long)(vendor & 0xFFFF) : -1;
		}
	}
	IOObjectRelease(iterator);
	CFRelease(classKey);
	CFRelease(vendorKey);
	return count;
}
*/
import "C"

import "fmt"

// Hex PCI vendor ids of every display controller, false when IOKit cannot be read
func displayVendorIDs() ([]string, bool) {
	var raw [256]C.longlong
	n := int(C.hostinfo_display_vendors(&raw[0], 256))
	if n < 0 {
		return nil, false
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if raw[i] < 0 {
			out = append(out, "")
		} else {
			out = append(out, fmt.Sprintf("%04x", int(raw[i])))
		}
	}
	return out, true
}
