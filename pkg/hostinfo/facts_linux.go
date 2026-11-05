package hostinfo

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

const wslNvidiaCUDA = "/usr/lib/wsl/lib/libcuda.so.1"

func nativeArchImpl() string { return ProcessArch() }

func cpuModelImpl() string {
	return ParseCPUInfo(readText("/proc/cpuinfo", 65536), readText("/proc/device-tree/model", 512))
}

func cpuVendorImpl() string { return ParseCPUInfoVendor(readText("/proc/cpuinfo", 65536)) }

func ramTotalImpl() int64 { return ParseMeminfoTotal(readText("/proc/meminfo", 4096)) }

// PCI vendor ids of DRM cards, nil when sysfs exposes no DRM tree
func linuxGPUVendorIDs() []string {
	root := "/sys/class/drm"
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) > gpuScanLimit*4 {
		names = names[:gpuScanLimit*4]
	}
	cards := []string{}
	for _, name := range names {
		if strings.HasPrefix(name, "card") && !strings.Contains(name, "-") {
			cards = append(cards, name)
		}
	}
	if len(cards) > gpuScanLimit {
		cards = cards[:gpuScanLimit]
	}
	vendors := []string{}
	for _, card := range cards {
		vendors = append(vendors, strings.TrimSpace(readText(root+"/"+card+"/device/vendor", 16)))
	}
	return vendors
}

func gpuClassImpl() string {
	if fileExists("/proc/driver/nvidia/version") || fileExists(wslNvidiaCUDA) {
		return "nvidia"
	}
	vendors := linuxGPUVendorIDs()
	if vendors == nil {
		return "unknown"
	}
	return ClassifyGPUVendors(vendors)
}

// Reports whether a path exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func nvidiaGPUsImpl() []NvidiaGPU {
	entries, err := os.ReadDir(nvidiaProcGPUs)
	if err != nil {
		return nil
	}
	buses := make([]string, 0, len(entries))
	for _, e := range entries {
		buses = append(buses, e.Name())
	}
	sort.Strings(buses)
	if len(buses) > gpuScanLimit {
		buses = buses[:gpuScanLimit]
	}
	var gpus []NvidiaGPU
	for _, bus := range buses {
		name := ParseNvidiaGPUModel(readText(nvidiaProcGPUs+"/"+bus+"/information", 4096))
		if name == "" {
			continue
		}
		device := strings.TrimPrefix(strings.TrimSpace(readText(sysPCIDevices+"/"+bus+"/device", 16)), "0x")
		gpus = append(gpus, NvidiaGPU{name, pciID(device)})
	}
	return gpus
}

func interactiveSessionImpl() bool {
	sessionID := strings.TrimSpace(readText("/proc/self/sessionid", 64))
	if sessionID == "" || sessionID == "4294967295" {
		return false
	}
	st, err := os.Stat("/run/user/" + strconv.Itoa(os.Getuid()))
	return err == nil && st.IsDir()
}
