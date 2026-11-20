//go:build darwin && cgo

package hostinfo

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/ps/IOPowerSources.h>
#include <IOKit/ps/IOPSKeys.h>

static int icebath_battery(int *capacity, int *plugged) {
	CFTypeRef info = IOPSCopyPowerSourcesInfo();
	if (info == NULL) {
		return 0;
	}
	CFArrayRef list = IOPSCopyPowerSourcesList(info);
	if (list == NULL) {
		CFRelease(info);
		return 0;
	}
	int found = 0;
	if (CFArrayGetCount(list) > 0) {
		CFDictionaryRef description = IOPSGetPowerSourceDescription(info, CFArrayGetValueAtIndex(list, 0));
		if (description != NULL) {
			CFNumberRef capacityRef = (CFNumberRef)CFDictionaryGetValue(description, CFSTR(kIOPSCurrentCapacityKey));
			CFStringRef stateRef = (CFStringRef)CFDictionaryGetValue(description, CFSTR(kIOPSPowerSourceStateKey));
			SInt32 value = 0;
			if (capacityRef != NULL && stateRef != NULL && CFNumberGetValue(capacityRef, kCFNumberSInt32Type, &value)) {
				*capacity = (int)value;
				*plugged = CFStringCompare(stateRef, CFSTR(kIOPSACPowerValue), 0) == kCFCompareEqualTo;
				found = 1;
			}
		}
	}
	CFRelease(list);
	CFRelease(info);
	return found;
}
*/
import "C"

// Reads the first IOKit power source the way psutil sensors_battery does on macOS
func platformSensorsBattery() (sensorReading, bool) {
	var capacity, pluggedFlag C.int
	if C.icebath_battery(&capacity, &pluggedFlag) == 0 {
		return sensorReading{}, false
	}
	percent := float64(capacity)
	plugged := pluggedFlag == 1
	return sensorReading{percent: &percent, plugged: &plugged}, true
}
