//go:build !linux && !windows

package fsutil

// The process umask, read by setting it since nothing else reports it here
func processUmask() uint32 { return swapUmask() }
