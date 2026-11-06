// Package hostinfo reads cached OS, architecture, CPU, memory, GPU and account facts of this machine
package hostinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var archNames = map[string]string{
	"amd64": "amd64", "x86_64": "amd64", "x64": "amd64",
	"arm64": "arm64", "aarch64": "arm64",
	"x86": "x86", "i386": "x86", "i686": "x86",
}

// ImageFileMachine maps PE machine codes to canonical architecture names
var ImageFileMachine = map[int]string{0x8664: "amd64", 0xAA64: "arm64", 0x014C: "x86"}

// NormalizeArch returns the canonical architecture name, unknown when unrecognized
func NormalizeArch(raw string) string {
	if name, ok := archNames[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return name
	}
	return "unknown"
}

// OSFamily is the platform identifier the way Python sys.platform names it
func OSFamily() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux", "android":
		return "linux"
	}
	return runtime.GOOS
}

// ProcessArch is the architecture this binary was built for
func ProcessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	}
	return "unknown"
}

// WindowsNativeArch is IsWow64Process2's native machine, else the reported machine name
func WindowsNativeArch(wow64Native int, machine string) string {
	if name, ok := ImageFileMachine[wow64Native]; ok && wow64Native != 0 {
		return name
	}
	return NormalizeArch(machine)
}

var nativeArchOnce = cached(func() string { return nativeArchImpl() })

// NativeArch is the machine architecture even when this process is emulated
func NativeArch() string { return nativeArchOnce() }

// Returns a function computing the value once and caching it until ClearCaches
func cached[T any](compute func() T) func() T {
	var mu sync.Mutex
	var value T
	done := false
	fn := func() T {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			value = compute()
			done = true
		}
		return value
	}
	cacheResets = append(cacheResets, func() {
		mu.Lock()
		done = false
		mu.Unlock()
	})
	return fn
}

var cacheResets []func()

// ClearCaches clears every cached host fact
func ClearCaches() {
	for _, reset := range cacheResets {
		reset()
	}
}

// ParseCPUInfo returns the first model name or Hardware line of cpuinfo, else the device tree model
func ParseCPUInfo(text, deviceTreeModel string) string {
	for _, key := range []string{"model name", "Hardware"} {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.ToLower(line), strings.ToLower(key)) && strings.Contains(line, ":") {
				return strings.TrimSpace(line[strings.Index(line, ":")+1:])
			}
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(deviceTreeModel, "\x00", ""), "_", " "))
}

// Reads up to limit bytes of a file, empty on any error, with a UTF-8 BOM stripped
func readText(path string, limit int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, _ := f.Read(buf)
	text := strings.TrimPrefix(string(buf[:n]), "\xEF\xBB\xBF")
	return strings.ToValidUTF8(text, "�")
}

var cpuModelOnce = cached(func() string { return cpuModelImpl() })

// CPUModel is the CPU model, empty when unavailable
func CPUModel() string { return cpuModelOnce() }

var armImplementers = map[int]string{
	0x41: "ARM", 0x42: "Broadcom", 0x43: "Cavium", 0x46: "Fujitsu", 0x48: "HiSilicon", 0x4E: "NVIDIA",
	0x50: "Ampere", 0x51: "Qualcomm", 0x61: "Apple", 0x69: "Intel", 0x6D: "Microsoft", 0xC0: "Ampere",
}

// ParseCPUInfoVendor returns vendor_id on x86 or the named CPU implementer on arm64 from cpuinfo text
func ParseCPUInfoVendor(text string) string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, ":"); i >= 0 {
			key := strings.TrimSpace(line[:i])
			if _, seen := fields[key]; !seen {
				fields[key] = strings.TrimSpace(line[i+1:])
			}
		}
	}
	if fields["vendor_id"] != "" {
		return fields["vendor_id"]
	}
	code, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(fields["CPU implementer"]), "0x"), 16, 64)
	if err != nil {
		return ""
	}
	return armImplementers[int(code)]
}

var cpuVendorOnce = cached(func() string { return cpuVendorImpl() })

// CPUVendor is the CPU vendor, empty when unavailable
func CPUVendor() string { return cpuVendorOnce() }

// ParseMeminfoTotal returns MemTotal of meminfo text in bytes, zero when absent
func ParseMeminfoTotal(text string) int64 {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}

var ramTotalOnce = cached(func() int64 { return ramTotalImpl() })

// RAMTotalBytes is the physical memory visible to this OS, zero when unavailable
func RAMTotalBytes() int64 { return ramTotalOnce() }

var gpuPCIVendors = map[string]string{"10de": "nvidia", "1002": "amd", "8086": "intel"}
var gpuPriority = []string{"nvidia", "amd", "intel"}

const gpuScanLimit = 16

// ClassifyGPUVendors returns the highest priority GPU class for PCI vendor ids, none, or unknown when none were readable
func ClassifyGPUVendors(vendorIDs []string) string {
	if len(vendorIDs) > 0 {
		readable := false
		for _, v := range vendorIDs {
			if strings.TrimSpace(v) != "" {
				readable = true
			}
		}
		if !readable {
			return "unknown"
		}
	}
	found := map[string]bool{}
	for _, v := range vendorIDs {
		found[gpuPCIVendors[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "0x")]] = true
	}
	for _, name := range gpuPriority {
		if found[name] {
			return name
		}
	}
	return "none"
}

// Four hex characters after a tag in a device id, empty when absent
func hexAfter(text, tag string) string {
	i := strings.Index(text, tag)
	if i < 0 {
		return ""
	}
	rest := text[i+len(tag):]
	if len(rest) > 4 {
		rest = rest[:4]
	}
	return rest
}

// DisplayAdapter is one Windows display class adapter row
type DisplayAdapter struct {
	VendorID    string
	DeviceID    string
	Description string
}

// ParseWindowsDisplayAdapters turns subkey, MatchingDeviceId and DriverDesc rows into adapters, skipping virtual ones
func ParseWindowsDisplayAdapters(rows [][3]string) []DisplayAdapter {
	var adapters []DisplayAdapter
	for _, row := range rows {
		subkey, matching, description := row[0], row[1], row[2]
		upper := strings.ToUpper(matching)
		if strings.Contains(upper, "VEN_") {
			adapters = append(adapters, DisplayAdapter{hexAfter(upper, "VEN_"), hexAfter(upper, "DEV_"), description})
		} else if isDigits(subkey) && matching == "" {
			adapters = append(adapters, DisplayAdapter{"", "", description})
		}
	}
	return adapters
}

// Reports whether the text is one or more ASCII digits
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var gpuClassOnce = cached(func() string { return gpuClassImpl() })

// GPUClass is nvidia, amd, intel, apple_silicon, none, or unknown
func GPUClass() string {
	if p := gpuClassPin.Load(); p != nil {
		return *p
	}
	return gpuClassOnce()
}

const (
	nvidiaProcGPUs  = "/proc/driver/nvidia/gpus"
	nvidiaPCIVendor = 0x10DE
	sysPCIDevices   = "/sys/bus/pci/devices"
)

// ParseNvidiaGPUModel returns the Model value of a nvidia gpus information file
func ParseNvidiaGPUModel(information string) string {
	for _, line := range strings.Split(information, "\n") {
		if strings.HasPrefix(line, "Model:") {
			return strings.TrimSpace(line[len("Model:"):])
		}
	}
	return ""
}

// NvidiaGPU is one NVIDIA GPU with its NVML packed PCI id, zero when unknown
type NvidiaGPU struct {
	Name  string
	PCIID int
}

// NVML packing of a device id above the 16 bit vendor id, zero when unparsable
func pciID(deviceHex string) int {
	n, err := strconv.ParseInt(strings.TrimSpace(deviceHex), 16, 64)
	if err != nil {
		return 0
	}
	return int(n)<<16 | nvidiaPCIVendor
}

// NvidiaGPUsFromAdapters is the NVIDIA entries of parsed Windows display adapters
func NvidiaGPUsFromAdapters(adapters []DisplayAdapter) []NvidiaGPU {
	var out []NvidiaGPU
	for _, a := range adapters {
		if strings.ToLower(a.VendorID) == "10de" {
			out = append(out, NvidiaGPU{a.Description, pciID(a.DeviceID)})
		}
	}
	return out
}

var nvidiaGPUsOnce = cached(func() []NvidiaGPU { return nvidiaGPUsImpl() })

// NvidiaGPUs is the name and PCI id of each NVIDIA GPU, empty without the driver or off Windows and Linux
func NvidiaGPUs() []NvidiaGPU { return nvidiaGPUsOnce() }

// InteractiveSession reports whether this process can reach an interactive user session
func InteractiveSession() bool {
	if p := interactiveSessionPin.Load(); p != nil {
		return *p
	}
	return interactiveSessionImpl()
}
