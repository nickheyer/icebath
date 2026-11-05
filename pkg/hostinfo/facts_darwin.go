package hostinfo

import (
	"golang.org/x/sys/unix"
)

// Reports whether this process runs under Rosetta
func darwinTranslated() bool {
	v, err := unix.SysctlUint32("sysctl.proc_translated")
	return err == nil && v == 1
}

func nativeArchImpl() string {
	if ProcessArch() == "amd64" && darwinTranslated() {
		return "arm64"
	}
	return ProcessArch()
}

func cpuModelImpl() string {
	s, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return ""
	}
	return s
}

func cpuVendorImpl() string {
	if NativeArch() == "arm64" {
		return "Apple"
	}
	s, err := unix.Sysctl("machdep.cpu.vendor")
	if err != nil {
		return ""
	}
	return s
}

func ramTotalImpl() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}

func gpuClassImpl() string {
	if NativeArch() == "arm64" {
		return "apple_silicon"
	}
	vendors, ok := displayVendorIDs()
	if !ok {
		return "unknown"
	}
	return ClassifyGPUVendors(vendors)
}

func nvidiaGPUsImpl() []NvidiaGPU { return nil }

func interactiveSessionImpl() bool { return true }
