package hardening

import "io/fs"

// Windows stat results carry no uid
func ownerUID(fs.FileInfo) (int, bool) { return 0, false }

// Windows has no process.getuid
func processUID() (int, bool) { return 0, false }
