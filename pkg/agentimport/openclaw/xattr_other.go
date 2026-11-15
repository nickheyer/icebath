//go:build !linux

package openclaw

// Copies nothing outside Linux, where shutil.copystat copies no extended attributes
func copyXattrs(src, dst string) error { return nil }
