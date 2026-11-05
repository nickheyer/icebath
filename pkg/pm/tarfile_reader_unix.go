//go:build !windows

package pm

import "syscall"

// Stores a value in a time field of either width, false when the platform time_t cannot hold it
func tarTimeField[T int32 | int64](field *T, v int64) bool {
	*field = T(v)
	return int64(*field) == v
}

// Sets access and modification time to one instant through utimensat the way os.utime does
func tarSetFileTimes(path string, sec, nsec int64) error {
	var ts syscall.Timespec
	if !tarTimeField(&ts.Sec, sec) {
		return &TarReadError{Class: "OverflowError", Message: "timestamp out of range for platform time_t"}
	}
	tarTimeField(&ts.Nsec, nsec)
	return syscall.UtimesNano(path, []syscall.Timespec{ts, ts})
}
