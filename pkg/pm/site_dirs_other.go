//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package pm

import "os"

// Stat on this platform records no hidden flag
func pthHidden(os.FileInfo) bool { return false }
