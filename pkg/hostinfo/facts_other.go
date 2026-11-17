//go:build !linux && !darwin && !windows

package hostinfo

func nativeArchImpl() string       { return ProcessArch() }
func ramTotalImpl() int64          { return 0 }
func gpuClassImpl() string         { return "unknown" }
func interactiveSessionImpl() bool { return true }
