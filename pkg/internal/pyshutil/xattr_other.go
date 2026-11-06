//go:build !linux

package pyshutil

// Copies nothing because shutil copies extended attributes only where os.listxattr exists
func copyXattr(src, dst string) error { return nil }
