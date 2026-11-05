package hostinfo

import "strings"

const rtxSparkCPUMarker = "N1X"

var dgxSparkMarkers = []string{"GB10", "DGX SPARK"}

// IsNvidiaN1XPCIID recognizes the N1X device range packed above the NVIDIA vendor id
func IsNvidiaN1XPCIID(pciID int) bool {
	return pciID&0xFFFF == 0x10DE && 0x2E00 <= pciID>>16 && pciID>>16 <= 0x2E3F
}

// ClassifyNvidiaPlatform returns rtx_spark, dgx_spark, discrete, or empty without an NVIDIA GPU
func ClassifyNvidiaPlatform(osFamily, nativeArch, cpuModel, gpuClass string, gpus []NvidiaGPU) string {
	cpu := strings.ToUpper(strings.ReplaceAll(cpuModel, "_", " "))
	if nativeArch == "arm64" && osFamily == "win32" {
		n1x := false
		for _, gpu := range gpus {
			if gpu.PCIID != 0 && IsNvidiaN1XPCIID(gpu.PCIID) {
				n1x = true
			}
		}
		if n1x || strings.Contains(cpu, rtxSparkCPUMarker) {
			return "rtx_spark"
		}
	}
	if nativeArch == "arm64" && strings.HasPrefix(osFamily, "linux") {
		names := make([]string, len(gpus))
		for i, gpu := range gpus {
			names[i] = gpu.Name
		}
		joined := strings.ToUpper(strings.Join(names, " "))
		for _, marker := range dgxSparkMarkers {
			if strings.Contains(joined, marker) || strings.Contains(cpu, marker) {
				return "dgx_spark"
			}
		}
	}
	if gpuClass == "nvidia" {
		return "discrete"
	}
	return ""
}

// NvidiaPlatform is the NVIDIA platform of this host from cached facts alone
func NvidiaPlatform() string {
	return ClassifyNvidiaPlatform(OSFamily(), NativeArch(), CPUModel(), GPUClass(), NvidiaGPUs())
}
