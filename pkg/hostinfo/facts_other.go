//go:build !linux && !darwin && !windows

package hostinfo

func nativeArchImpl() string       { return ProcessArch() }
func cpuModelImpl() string         { return "" }
func cpuVendorImpl() string        { return "" }
func ramTotalImpl() int64          { return 0 }
func gpuClassImpl() string         { return "unknown" }
func nvidiaGPUsImpl() []NvidiaGPU  { return nil }
func interactiveSessionImpl() bool { return true }
