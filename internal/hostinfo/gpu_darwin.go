package hostinfo

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

const (
	pciDisplayClass  = 0x03
	ioKitDeviceLimit = 256
)

// No nvidia-smi install location exists outside PATH on macOS
func smiCandidates() []string { return nil }

// Apple silicon by architecture, else the vendors of the IOKit PCI display controllers
func gpuClass(h *Host) GPUClass {
	if h.NativeArch() == ArchARM64 {
		return GPUAppleSilicon
	}
	vendors, err := displayVendorIDs()
	if err != nil {
		return GPUUnknown
	}
	return classifyGPUVendors(vendors)
}

// No NVIDIA adapter listing exists on macOS, whose platform classification never reads one
func nvidiaGPUs(*Host) []nvidiaGPU { return nil }

// Hex PCI vendor id of every display class IOKit PCI device, empty for a device whose vendor is unreadable
func displayVendorIDs() ([]string, error) {
	f, err := loadFrameworks()
	if err != nil {
		return nil, err
	}
	class := []byte("IOPCIDevice\x00")
	matching := f.call("IOServiceMatching", uintptr(unsafe.Pointer(&class[0])))
	runtime.KeepAlive(class)
	if matching == 0 {
		return nil, errors.New("iokit: no matching dictionary for IOPCIDevice")
	}
	var iterator uint32
	if r := f.call("IOServiceGetMatchingServices", 0, matching, uintptr(unsafe.Pointer(&iterator))); int32(r) != 0 {
		return nil, fmt.Errorf("iokit: IOServiceGetMatchingServices kern_return %d", int32(r))
	}
	defer f.call("IOObjectRelease", uintptr(iterator))
	classKey, vendorKey := f.cfString("class-code"), f.cfString("vendor-id")
	defer f.release(classKey)
	defer f.release(vendorKey)
	property := func(entry, key uintptr) []byte {
		value := f.call("IORegistryEntryCreateCFProperty", entry, key, 0, 0)
		defer f.release(value)
		data, _ := f.data(value)
		return data
	}
	vendors := []string{}
	for range ioKitDeviceLimit {
		entry := f.call("IOIteratorNext", uintptr(iterator))
		if uint32(entry) == 0 {
			break
		}
		code, vendor := property(entry, classKey), property(entry, vendorKey)
		f.call("IOObjectRelease", entry)
		if c, ok := littleEndian(code); !ok || c>>16 != pciDisplayClass {
			continue
		}
		id := ""
		if v, ok := littleEndian(vendor); ok {
			id = fmt.Sprintf("%04x", v&0xFFFF)
		}
		vendors = append(vendors, id)
	}
	return vendors, nil
}
