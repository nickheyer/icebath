package selfupdate

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// Sets the access and modification times of st on the open file itself to the nanosecond
func utimeHandle(out *os.File, st os.FileInfo) error {
	times := []unix.Timespec{unix.NsecToTimespec(pyshutil.FileAtime(st).UnixNano()), unix.NsecToTimespec(st.ModTime().UnixNano())}
	return unix.UtimesNanoAt(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(int(out.Fd())), times, 0)
}
