package home

import (
	"errors"
	"io/fs"
)

var errNoPOSIXIDs = errors.New("stuck entry tracking needs POSIX uids")

// Windows keeps no stuck records, so no fingerprint is ever compared
func entryFingerprint(string) ([]int64, bool) { return nil, false }

// Windows keeps no stuck records, so no blocker is ever probed
func accessibleNow(string) bool { return true }

// Windows has no flock based stuck state
func lockStuck(string, func()) error { return errNoPOSIXIDs }

// Windows directory entries report no hard link count
func linkCount(fs.FileInfo) uint64 { return 0 }
