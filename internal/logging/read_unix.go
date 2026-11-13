//go:build unix

package logging

import "os"

// Log opened for reading, a POSIX open never standing in the way of a rotation
func openShared(path string) (*os.File, error) { return os.Open(path) }
