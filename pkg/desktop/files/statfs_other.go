//go:build !linux

package files

import "errors"

// Only Linux mounts the kernel pseudo file systems the door refuses by type
func statfsType(string) (int64, error) { return 0, errors.ErrUnsupported }

// Kernel pseudo file systems are a Linux concept
func onPseudoFilesystem(string) bool { return false }
