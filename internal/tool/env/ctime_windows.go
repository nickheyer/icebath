package env

import (
	"io/fs"
	"syscall"
)

func changeTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return st.CreationTime.Nanoseconds()
	}
	return 0
}
