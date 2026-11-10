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
	var perm os.FileMode
	permSet := false
	if mode != nil {
		perm, permSet = *mode, true
	} else if st, err := os.Stat(path); err == nil {
		perm, permSet = st.Mode().Perm(), true
	} else {
		perm, permSet = defaultNewFileMode()
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
	if permSet && canFchmod {
		if err := tmp.Chmod(perm); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(content); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
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
	if permSet {
		os.Chmod(publish, perm)
	}
	if fsyncDir {
		FsyncDirectory(filepath.Dir(publish))
	}
	return nil
}
