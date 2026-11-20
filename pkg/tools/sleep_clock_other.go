//go:build !windows

package tools

// Nil since no kernel32 exists here to answer the unbiased interrupt time
func windowsUnbiasedClock() func() float64 { return nil }
