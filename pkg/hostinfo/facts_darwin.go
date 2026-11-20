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

func interactiveSessionImpl() bool { return true }
