package pyfs

// Owning uid and gid of a file on a POSIX host
type FileOwner struct{ UID, GID int }

// Gives a replaced file back to its previous owner, ignoring a refusal, a nil owner leaving it alone
func RestoreFileOwner(path string, owner *FileOwner) {
	if owner != nil {
		chownPath(path, owner.UID, owner.GID)
	}
}

// Applies permission bits to path when a mode was captured, ignoring a refusal
func RestoreFileMode(path string, mode uint32, ok bool) {
	if ok {
		chmodPath(path, mode)
	}
}
