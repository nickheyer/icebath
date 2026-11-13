package hostinfo

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	drmRoot                = "/sys/class/drm"
	nvidiaProcGPUs         = "/proc/driver/nvidia/gpus"
	nvidiaDriverVersion    = "/proc/driver/nvidia/version"
	wslNvidiaCUDA          = "/usr/lib/wsl/lib/libcuda.so.1"
	sysPCIDevices          = "/sys/bus/pci/devices"
	nvidiaInformationLimit = 4096
)

// Places nvidia-smi lives outside PATH, the WSL driver bridge on Linux
func smiCandidates() []string { return []string{"/usr/lib/wsl/lib/nvidia-smi"} }

// NVIDIA by its driver procfs or the WSL GPU bridge, else the vendors of the DRM cards
func gpuClass(*Host) GPUClass {
	if exists(nvidiaDriverVersion) || exists(wslNvidiaCUDA) {
		return GPUNvidia
	}
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		return GPUUnknown
	}
	vendors := []string{}
	for i, entry := range entries {
		name := entry.Name()
		if i >= gpuScanLimit*4 || len(vendors) >= gpuScanLimit {
			break
		}
		if strings.HasPrefix(name, "card") && !strings.Contains(name, "-") {
			vendors = append(vendors, strings.TrimSpace(readText(filepath.Join(drmRoot, name, "device", "vendor"), 16)))
		}
	}
	return classifyGPUVendors(vendors)
}

// GPUs the NVIDIA driver lists in procfs with a model name and sysfs device ids, none without a readable list
func nvidiaGPUs(*Host) []nvidiaGPU {
	entries, err := os.ReadDir(nvidiaProcGPUs)
	if err != nil {
		return nil
	}
	var gpus []nvidiaGPU
	for i, entry := range entries {
		if i >= gpuScanLimit {
			break
		}
		bus := entry.Name()
		name := nvidiaModel(readText(filepath.Join(nvidiaProcGPUs, bus, "information"), nvidiaInformationLimit))
		if name == "" {
			continue
		}
		gpus = append(gpus, nvidiaGPU{Name: name, PCIID: nvidiaPCIID(readText(filepath.Join(sysPCIDevices, bus, "device"), 16))})
	}
	return gpus
}

// Model value of an NVIDIA driver information file
func nvidiaModel(information string) string {
	for _, line := range strings.Split(information, "\n") {
		if model, ok := strings.CutPrefix(line, "Model:"); ok {
			return strings.TrimSpace(model)
		}
	}
	return ""
}
