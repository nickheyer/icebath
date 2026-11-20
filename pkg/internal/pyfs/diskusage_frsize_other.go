//go:build !windows && !linux

package pyfs

import "golang.org/x/sys/unix"

// Fragment size statvfs reports, the fundamental block size on BSD derived systems
func statvfsFragmentSize(st *unix.Statfs_t) uint64 { return uint64(st.Bsize) }
