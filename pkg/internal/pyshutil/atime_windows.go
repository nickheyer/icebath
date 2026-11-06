package pyshutil

import (
	"os"
	"syscall"
	"time"
)

// Access time of a file from its attribute data
func fileAtime(fi os.FileInfo) time.Time {
	d := fi.Sys().(*syscall.Win32FileAttributeData)
	return time.Unix(0, d.LastAccessTime.Nanoseconds())
}
