package connections

import (
	"time"

	"golang.org/x/sys/unix"
)

// Birth time of a file from its stat record
func fileBirthtime(path string) (time.Time, bool) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return time.Time{}, false
	}
	return time.Unix(st.Btim.Unix()), true
}
