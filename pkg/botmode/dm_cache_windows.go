//go:build windows

package botmode

import "io/fs"

// No user id on Windows, where os.getuid does not exist
func currentUID() (int, bool) { return 0, false }

// No owning user id on Windows
func ownerUID(fs.FileInfo) (int, bool) { return 0, false }
