//go:build windows

package browser

import "io/fs"

// Windows has no uid so no existing dir counts as owned by this process
func ownedPrivateDir(info fs.FileInfo) bool { return false }

func ownedWithoutGroupWrite(info fs.FileInfo) bool { return true }
