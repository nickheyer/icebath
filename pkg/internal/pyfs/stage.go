package pyfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// Temp file in the directory a later AtomicReplace publishes into, beside the link when that one is unwritable
func MkstempBeside(target, prefix, suffix string) (*os.File, error) {
	linkDir := filepath.Dir(target)
	stageDir := filepath.Dir(publishPath(target))
	tmp, err := os.CreateTemp(stageDir, prefix+"*"+suffix)
	if err != nil && errors.Is(err, os.ErrPermission) && stageDir != linkDir {
		return os.CreateTemp(linkDir, prefix+"*"+suffix)
	}
	return tmp, err
}

// Moves tmp onto target keeping a symlink target in place, returning the path actually written
func AtomicReplace(tmp, target string) (string, error) {
	publish := publishPath(target)
	return publish, atomicReplace(tmp, publish)
}

// Directory flush every durable write calls, the name a case replaces the way pytest patches utils.fsync_directory
var FsyncDirectory = fsyncDirectory

// Flushes a directory entry to disk, silently skipped on Windows and on any open or sync error
func fsyncDirectory(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(path)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
