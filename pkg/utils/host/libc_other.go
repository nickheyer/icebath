//go:build unix && !linux

package hostutil

// No C library part in the label of a Unix system other than Linux
func libcLabel() string { return "" }
