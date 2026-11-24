package fsutil

import "io/fs"

// Owning user and group ids of a file on a POSIX host
type Owner struct{ UID, GID int }

// Hands path to owner, ignoring a refusal; a nil owner leaves it alone
func RestoreOwner(path string, owner *Owner) {
	if owner != nil {
		chown(path, owner.UID, owner.GID)
	}
}

// Sets all twelve permission bits of path, setuid, setgid and sticky given in their POSIX positions; on Windows only the owner write bit counts, as the read only attribute
func SetPermBits(path string, mode uint32) error { return chmodBits(path, mode) }

// POSIX permission bits of a FileMode, its setuid, setgid and sticky flags moved into their positions
func posixBits(m fs.FileMode) uint32 {
	bits := uint32(m) & 0o7777
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}
