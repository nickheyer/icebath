package hostinfo

import (
	"strconv"
	"strings"
)

// Vendor family of the strongest graphics adapter, apple_silicon by architecture
type GPUClass string

const (
	GPUNvidia       GPUClass = "nvidia"
	GPUAMD          GPUClass = "amd"
	GPUIntel        GPUClass = "intel"
	GPUAppleSilicon GPUClass = "apple_silicon"
	GPUNone         GPUClass = "none"
	GPUUnknown      GPUClass = "unknown"
)

const (
	gpuScanLimit    = 16
	nvidiaPCIVendor = 0x10DE
)

var (
	gpuPCIVendors = map[string]GPUClass{"10de": GPUNvidia, "1002": GPUAMD, "8086": GPUIntel}
	gpuPriority   = []GPUClass{GPUNvidia, GPUAMD, GPUIntel}
)

// Highest priority class among PCI vendor ids, none without one, unknown when every id seen was unreadable
func classifyGPUVendors(vendorIDs []string) GPUClass {
	found := map[GPUClass]bool{}
	readable := len(vendorIDs) == 0
	for _, id := range vendorIDs {
		id = strings.TrimSpace(id)
		readable = readable || id != ""
		found[gpuPCIVendors[strings.TrimPrefix(strings.ToLower(id), "0x")]] = true
	}
	if !readable {
		return GPUUnknown
	}
	for _, class := range gpuPriority {
		if found[class] {
			return class
		}
	}
	return GPUNone
}

// Vendor class of the graphics adapters, read once, unknown when the adapters cannot be read
func (h *Host) GPUClass() GPUClass { return h.gpuClass.value(func() GPUClass { return gpuClass(h) }) }

// One NVIDIA adapter with its PCI id packed above the vendor id as NVML packs it
type nvidiaGPU struct {
	Name  string
	PCIID *uint32
}

// NVIDIA adapters the Linux driver or the Windows device registry names, read once, empty when unreadable or on macOS
func (h *Host) nvidiaAdapters() []nvidiaGPU {
	return h.nvidia.value(func() []nvidiaGPU { return nvidiaGPUs(h) })
}

// Packed PCI id of a hex NVIDIA device id, nil when it does not parse
func nvidiaPCIID(deviceHex string) *uint32 {
	digits := strings.TrimSpace(deviceHex)
	if len(digits) > 2 && digits[0] == '0' && (digits[1] == 'x' || digits[1] == 'X') {
		digits = digits[2:]
	}
	device, err := strconv.ParseUint(digits, 16, 16)
	if err != nil {
		return nil
	}
	id := uint32(device)<<16 | nvidiaPCIVendor
	return &id
}

// NVIDIA product family of a host, empty without an NVIDIA GPU
type NvidiaPlatform string

const (
	RTXSpark       NvidiaPlatform = "rtx_spark"
	DGXSpark       NvidiaPlatform = "dgx_spark"
	NvidiaDiscrete NvidiaPlatform = "discrete"
)

var dgxSparkMarkers = []string{"GB10", "DGX SPARK"}

// Reports a packed PCI id in the N1X device range of the NVIDIA vendor
func IsNvidiaN1XPCIID(id uint32) bool {
	return id&0xFFFF == nvidiaPCIVendor && id>>16 >= 0x2E00 && id>>16 <= 0x2E3F
}

// NVIDIA product family of this host from its cached facts alone
func (h *Host) nvidiaPlatform() NvidiaPlatform {
	return classifyNvidiaPlatform(OSFamily, h.NativeArch(), h.processorModel(), h.GPUClass(), h.nvidiaAdapters())
}

// RTX Spark for N1X Windows arm64, DGX Spark for GB10 Linux arm64, else discrete when the GPU class is NVIDIA
func classifyNvidiaPlatform(family Family, arch Arch, cpuModel string, class GPUClass, gpus []nvidiaGPU) NvidiaPlatform {
	cpu := strings.ToUpper(strings.ReplaceAll(cpuModel, "_", " "))
	if arch == ArchARM64 && family == FamilyWindows {
		for _, gpu := range gpus {
			if gpu.PCIID != nil && IsNvidiaN1XPCIID(*gpu.PCIID) {
				return RTXSpark
			}
		}
		if strings.Contains(cpu, "N1X") {
			return RTXSpark
		}
	}
	if arch == ArchARM64 && family == FamilyLinux {
		names := make([]string, len(gpus))
		for i, gpu := range gpus {
			names[i] = gpu.Name
		}
		joined := strings.ToUpper(strings.Join(names, " "))
		for _, marker := range dgxSparkMarkers {
			if strings.Contains(joined, marker) || strings.Contains(cpu, marker) {
				return DGXSpark
			}
		}
	}
	if class == GPUNvidia {
		return NvidiaDiscrete
	}
	return ""
}

// One graphics adapter as DXGI enumerates it on Windows
type DXGIAdapter struct {
	Description    string
	VendorID       uint32
	DedicatedBytes uint64
	SharedBytes    uint64
	Software       bool
}

// Memory Windows lets the adapter allocate, dedicated plus shared as Task Manager shows it
func (a DXGIAdapter) MemoryBytes() uint64 { return a.DedicatedBytes + a.SharedBytes }
