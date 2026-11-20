package pyfs

import (
	"context"
	"os"
)

// Writes bytes through a fsynced temp sibling and an atomic rename, mode nil keeping an existing file mode
func AtomicWriteBytes(ctx context.Context, path string, content []byte, prefix string, mode *os.FileMode, fsyncDir bool) error {
	if prefix == "" {
		prefix = ".tmp_"
	}
	var perm uint32
	var permSet bool
	if mode != nil {
		perm, permSet = PosixModeBits(*mode), true
	} else {
		perm, permSet = PreserveFileMode(path)
	}
	write := func(f *os.File) error {
		_, err := f.Write(content)
		return err
	}
	return AtomicWrite(ctx, path, write, WriteOptions{Prefix: prefix, Mode: perm, ModeSet: permSet, FsyncDir: fsyncDir})
}
