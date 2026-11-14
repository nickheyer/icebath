package env

import (
	"io/fs"
	"syscall"
)

func changeTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1_000_000_000 + st.Ctim.Nsec
	}
	return 0
}
