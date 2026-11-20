//go:build !windows

package hostinfo

func windowsGPUAdaptersImpl() []GPUAdapter { return nil }
