//go:build !(darwin || freebsd || netbsd || openbsd || dragonfly)

package pyfs

import "os"

// Stats the path as shutil does, never immutable where stat results carry no st_flags
func isImmutable(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	return false, nil
}
