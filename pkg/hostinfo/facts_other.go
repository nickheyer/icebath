//go:build !linux && !darwin && !windows

package hostinfo

func nativeArchImpl() string       { return ProcessArch() }
func gpuClassImpl() string         { return "unknown" }
func interactiveSessionImpl() bool { return true }
