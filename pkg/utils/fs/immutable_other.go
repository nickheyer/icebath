//go:build !(darwin || freebsd || netbsd || openbsd || dragonfly)

package fsutil

import "os"

// Stats path, never immutable where stat results carry no file flags
func isImmutable(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	return false, nil
}
