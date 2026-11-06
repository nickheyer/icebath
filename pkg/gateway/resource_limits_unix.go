//go:build unix

package gateway

import "syscall"

var nofileSupported = true

// Open file soft and hard limits of this process
var getNofile = func() (uint64, uint64, error) {
	var r syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r)
	return uint64(r.Cur), uint64(r.Max), err
}

// Sets the open file soft and hard limits of this process
var setNofile = func(soft, hard uint64) error {
	var r syscall.Rlimit
	setRlimField(&r.Cur, soft)
	setRlimField(&r.Max, hard)
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &r)
}

// Stores a limit into an rlim_t field whatever its signedness
func setRlimField[T int64 | uint64](dst *T, v uint64) { *dst = T(v) }
