//go:build !windows

package config

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Warns through log with a chmod hint when a credential file is group or world readable, reporting whether it warned
func WarnBroadlyReadable(path, label string, log *slog.Logger) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o044 == 0 {
		return false
	}
	mode := uint32(fi.Mode().Perm())
	for bit, flag := range map[uint32]fs.FileMode{0o4000: fs.ModeSetuid, 0o2000: fs.ModeSetgid, 0o1000: fs.ModeSticky} {
		if fi.Mode()&flag != 0 {
			mode |= bit
		}
	}
	if label != "" {
		label += " "
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn(fmt.Sprintf("%s%s is group/world-readable (mode 0%o) and contains secrets. Run: chmod 600 %s", label, filepath.Base(path), mode, path))
	return true
}
