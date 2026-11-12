package pyfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
)

// Writes bytes through a fsynced temp sibling and an atomic rename, mode nil keeping an existing file mode
func AtomicWriteBytes(ctx context.Context, path string, content []byte, prefix string, mode *os.FileMode, fsyncDir bool) error {
	if prefix == "" {
		prefix = ".tmp_"
	}
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	var perm uint32
	permSet := false
	if mode != nil {
		perm, permSet = posixModeBits(*mode), true
	} else if perm, permSet = PreserveFileMode(path); !permSet {
		var fresh os.FileMode
		fresh, permSet = defaultNewFileMode()
		perm = posixModeBits(fresh)
	}
	publish := publishPath(path)
	linkDir := filepath.Dir(path)
	stageDir := filepath.Dir(publish)
	tmp, err := os.CreateTemp(stageDir, prefix+"*.tmp")
	if err != nil && errors.Is(err, os.ErrPermission) && stageDir != linkDir {
		tmp, err = os.CreateTemp(linkDir, prefix+"*.tmp")
	}
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if permSet {
		if err := fchmodStaged(tmp, perm); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(content); err != nil {
		return fail(err)
	}
	if err := Fsync(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := atomicReplace(tmpName, publish); err != nil {
		os.Remove(tmpName)
		return err
	}
	restoreFileMetadata(publish, nil, perm, permSet)
	if fsyncDir {
		FsyncDirectory(filepath.Dir(publish))
	}
	return nil
}
