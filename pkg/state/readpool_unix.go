//go:build unix

package state

import "golang.org/x/sys/unix"

// RLIM_INFINITY as the signed value Python reports, -1 where the C constant is all ones
var rlimInfinity = func() int64 { v := uint64(unix.RLIM_INFINITY); return int64(v) }()

// Soft RLIMIT_NOFILE read through getrlimit as the signed value Python's resource module reports
var rlimitNofile = func() (int64, error) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil {
		return 0, err
	}
	return int64(rl.Cur), nil
}
