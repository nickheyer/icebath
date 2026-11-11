//go:build unix

package selfupdate

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const openNoFollow = syscall.O_NOFOLLOW

// Sets the permission, setuid, setgid and sticky bits of st on the open file itself
func chmodHandle(out *os.File, st os.FileInfo) error {
	mode := uint32(st.Mode().Perm())
	if st.Mode()&os.ModeSetuid != 0 {
		mode |= unix.S_ISUID
	}
	if st.Mode()&os.ModeSetgid != 0 {
		mode |= unix.S_ISGID
	}
	if st.Mode()&os.ModeSticky != 0 {
		mode |= unix.S_ISVTX
	}
	return unix.Fchmod(int(out.Fd()), mode)
}
