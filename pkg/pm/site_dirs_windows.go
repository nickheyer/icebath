package pm

import (
	"os"
	"syscall"
)

// Reports the hidden file attribute Windows records
func pthHidden(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && sys.FileAttributes&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}
