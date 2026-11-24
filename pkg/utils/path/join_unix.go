//go:build !windows

package pathutil

import (
	"path/filepath"
	"strings"
)

// Elements joined like filepath.Join, except that an absolute element replaces everything before it
func Join(base string, elem ...string) string {
	for i := len(elem) - 1; i >= 0; i-- {
		if strings.HasPrefix(elem[i], "/") {
			return filepath.Join(elem[i:]...)
		}
	}
	switch len(elem) {
	case 0:
		return filepath.Join(base)
	case 1:
		return filepath.Join(base, elem[0])
	}
	all := make([]string, 0, len(elem)+1)
	return filepath.Join(append(append(all, base), elem...)...)
}
