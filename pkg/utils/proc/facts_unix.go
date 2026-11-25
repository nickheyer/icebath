//go:build !windows

package proc

import (
	"path/filepath"
	"strings"
)

// Process name like psutil, a 15 byte kernel name widened to the argv0 basename it truncates
func Name(pid int) (string, error) {
	name, err := comm(pid)
	if err != nil || len(name) < 15 {
		return name, err
	}
	if argv, err := Cmdline(pid); err == nil && len(argv) > 0 {
		if extended := filepath.Base(argv[0]); strings.HasPrefix(extended, name) {
			return extended, nil
		}
	}
	return name, nil
}
