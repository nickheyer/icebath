//go:build windows

package logging

import "os"

// Windows files carry no uid and gid to hand back
func fileOwner(os.FileInfo) (int, int, bool) { return 0, 0, false }
