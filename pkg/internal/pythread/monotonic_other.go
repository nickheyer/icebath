//go:build !unix && !windows

package pythread

// Zero on a platform with no boot relative clock, readings then count from process start
func systemMonotonic() float64 { return 0 }
