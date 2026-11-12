package connections

import (
	"time"

	"golang.org/x/sys/unix"
)

// Birth time of a file through statx, false when the filesystem does not record one
func fileBirthtime(path string) (time.Time, bool) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &st); err != nil || st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec)), true
}
