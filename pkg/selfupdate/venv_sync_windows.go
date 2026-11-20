//go:build windows && !icebath_core && !icebath_noextras

package selfupdate

import "os"

// Windows has no uid ownership, the foreign owner refusal never runs there
func defaultLstatOwner(path string) (int, error) {
	_, err := os.Lstat(path)
	return os.Geteuid(), err
}
