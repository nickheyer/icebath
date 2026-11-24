//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"os"
	"strconv"

	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	"golang.org/x/sys/unix"
)

// Sets the access and modification times of st on the open file itself to the nanosecond
func utimeHandle(out *os.File, st os.FileInfo) error {
	times := []unix.Timespec{unix.NsecToTimespec(fsutil.Atime(st).UnixNano()), unix.NsecToTimespec(st.ModTime().UnixNano())}
	return unix.UtimesNanoAt(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(int(out.Fd())), times, 0)
}
