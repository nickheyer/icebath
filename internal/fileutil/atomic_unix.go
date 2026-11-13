//go:build unix

package fileutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"syscall"
)

// POSIX renames leave directory durability to a separate directory fsync
const renameWritesThrough = false

// Owning uid and gid of a file a replace hands back
type fileOwner struct{ uid, gid int }

// Temporary file in dir, owner-only from creation
func createTemp(dir string, cfg writeConfig) (*os.File, error) {
	return os.CreateTemp(dir, cfg.prefix+"*"+cfg.suffix)
}

// Sets the planned mode on the open temp file before any byte lands in it
func applyStagedMode(f *os.File, plan metaPlan) error {
	if !plan.modeSet {
		return nil
	}
	return f.Chmod(plan.mode)
}

// Mode a temporary file is created with, which a replaced target keeps without KeepPerm or Perm
func stagedTempMode() (fs.FileMode, bool) { return 0o600, true }

// Mode a plain create gives a new file under the process umask, read through a restrictive swap
func defaultNewFileMode() (fs.FileMode, bool) {
	mask := syscall.Umask(0o077)
	syscall.Umask(mask)
	return fs.FileMode(0o666 &^ mask), true
}

// Owner recorded in a stat result
func ownerOf(fi fs.FileInfo) *fileOwner {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &fileOwner{uid: int(st.Uid), gid: int(st.Gid)}
}

// Chowns path back to owner when it differs and the process holds that right, a refusal keeping the owner
func restoreOwner(path string, owner *fileOwner) {
	if owner == nil {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if now := ownerOf(fi); (now == nil || *now != *owner) && mayChown(*owner) {
		_ = os.Chown(path, owner.uid, owner.gid)
	}
}

// Reports whether POSIX lets this process hand a file to o, root always and others only within their own groups
func mayChown(o fileOwner) bool {
	euid := os.Geteuid()
	if euid == 0 {
		return true
	}
	if o.uid != euid {
		return false
	}
	if o.gid == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	return err != nil || slices.Contains(groups, o.gid)
}

// Reports whether POSIX lets this process chmod path, root always and others only on files they own
func mayChmod(path string) bool {
	euid := os.Geteuid()
	if euid == 0 {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == euid
}

// Gives a kept inode the staged mode before new bytes land, a private write refusing one it cannot secure
func secureKeptInode(f *os.File, _ string, mode fs.FileMode, private bool) error {
	if err := f.Chmod(mode); err != nil && private {
		return err
	}
	return nil
}

// Renames tmp over the resolved target, copying through the target inode across devices and bind mounts
func replace(_ context.Context, tmp, target string, _, private bool) (string, bool, error) {
	published, err := publishPath(target)
	if err != nil {
		return "", false, err
	}
	err = os.Rename(tmp, published)
	if err == nil {
		return published, false, nil
	}
	if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EBUSY) {
		return "", false, err
	}
	if err := copyOver(tmp, published, private); err != nil {
		return "", false, err
	}
	return published, true, nil
}

// Flushes a directory so entries created, renamed or removed inside it survive power loss, every failure reported
func FsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return joinErr(d.Sync(), d.Close())
}
