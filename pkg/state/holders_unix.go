//go:build unix

package state

import (
	"errors"
	"os"
	"syscall"
)

// Directory, link and stat primitives the holder scan uses, swapped by tests projecting a fake procfs
type holderSys struct {
	listdir  func(string) ([]string, error)
	readlink func(string) (string, error)
	stat     func(string) (FileIdentity, error)
}

var holdersSys = holderSys{listdir: listdirUnordered, readlink: os.Readlink, stat: statIdentity}

// Directory entries in the order the kernel returns them, like os.listdir
func listdirUnordered(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// Device and inode of the file path names after following symlinks, with the stat error
func statIdentity(path string) (FileIdentity, error) {
	var st syscall.Stat_t
	err := syscall.Stat(path, &st)
	for err == syscall.EINTR {
		err = syscall.Stat(path, &st)
	}
	if err != nil {
		return FileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// Whether an OSError carries ENOENT or ESRCH, the errors of a descriptor or process that vanished
func isVanishedErr(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH)
}

// Stats the database family by device and inode, then scans every other process for descriptors on it
func foreignStateDBHolders(dbPath string) ([]Holder, error) {
	dbPathStr, err := pyRealpath(dbPath)
	if err != nil {
		return nil, err
	}
	watched := map[string]bool{}
	for _, candidate := range sqliteFamily(dbPathStr) {
		canonical, err := CanonicalSQLitePath(candidate)
		if err != nil {
			return nil, err
		}
		watched[canonical] = true
	}
	holders := []Holder{}
	watchedIDs := map[FileIdentity]bool{}
	var dbDev uint64
	hasDBDev := false
	for _, candidate := range sqliteFamily(dbPathStr) {
		id, err := holdersSys.stat(candidate)
		if err != nil {
			if !isVanishedErr(err) {
				holders = append(holders, Holder{-1, "watched-file stat failed: " + candidate + ": " + pyOSErrorStr(err)})
			}
			continue
		}
		watchedIDs[id] = true
		if candidate == dbPathStr {
			dbDev, hasDBDev = id.Dev, true
		}
	}
	return scanOpenFiles(dbPath, watched, watchedIDs, dbDev, hasDBDev, holders), nil
}
