//go:build !linux

package fsutil

// Extended attributes are carried by copies on Linux only
func copyXattr(_, _ string) error { return nil }

// Extended attributes of links are carried by copies on Linux only
func copyLinkXattr(_, _ string) error { return nil }
