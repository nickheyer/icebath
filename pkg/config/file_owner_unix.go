//go:build unix

package config

import (
	"os"
	"syscall"
)

type fileOwnerID struct {
	uid, gid int
	ok       bool
}

// Owner of a stat result
func fileOwnerOf(fi os.FileInfo) fileOwnerID {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileOwnerID{}
	}
	return fileOwnerID{uid: int(st.Uid), gid: int(st.Gid), ok: true}
}

// Gives a replaced file back to its previous owner, ignoring a refusal as an unprivileged process gets
func restoreFileOwner(path string, owner fileOwnerID) {
	if owner.ok {
		_ = os.Chown(path, owner.uid, owner.gid)
	}
}

// Mode a plain create gives a new file under the process umask
func defaultNewFileMode() (os.FileMode, bool) {
	current := syscall.Umask(0o077)
	syscall.Umask(current)
	return os.FileMode(0o666 &^ current), true
}
