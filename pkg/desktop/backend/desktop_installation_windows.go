//go:build windows

package backend

import "os"

// Windows has no process uid, so every file counts as this user's
func ownedByOtherUser(os.FileInfo) bool { return false }
