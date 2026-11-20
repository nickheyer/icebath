package pyfs

import "os"

// Owning uid and gid of a file on a POSIX host
type FileOwner struct{ UID, GID int }

// Reapplies a captured owner and permission bits after an atomic replace, ignoring each refusal
func restoreFileMetadata(path string, owner *FileOwner, mode uint32, modeOK bool) {
	if owner != nil {
		chownPath(path, owner.UID, owner.GID)
	}
	if modeOK {
		chmodPath(path, mode)
	}
}

// Gives a replaced file back to its previous owner, ignoring a refusal, a nil owner leaving it alone
func RestoreFileOwner(path string, owner *FileOwner) {
	restoreFileMetadata(path, owner, 0, false)
}

// Applies permission bits to path when a mode was captured, ignoring a refusal
func RestoreFileMode(path string, mode uint32, ok bool) {
	restoreFileMetadata(path, nil, mode, ok)
}

// Octal permission bits of a FileMode with the setuid, setgid and sticky flags folded into their positions
func PosixModeBits(m os.FileMode) uint32 {
	bits := uint32(m) & 0o7777
	if m&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}
