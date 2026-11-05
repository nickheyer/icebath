//go:build !windows && !linux

package cron

import "golang.org/x/sys/unix"

// Fragment size statvfs reports, which these kernels expose as the statfs block size
func fragmentSize(st *unix.Statfs_t) uint64 { return uint64(st.Bsize) }
