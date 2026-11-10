//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package pm

import (
	"os"
	"syscall"
)

// Reports the UF_HIDDEN file flag the platform stat records
func pthHidden(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Flags&0x00008000 != 0
}
