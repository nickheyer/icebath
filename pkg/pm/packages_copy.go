package pm

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Copies a file's bytes and metadata into dst or into dst's directory the way shutil copy2 does
func copy2(src, dst string) error {
	if isDirectory(dst) {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	return platformCopy2(src, dst)
}

// Copies a file's bytes the way shutil copyfile does, refusing the same file and named pipes
func copyFileData(src, dst string) error {
	if srcSt, err := os.Stat(src); err == nil {
		if dstSt, err := os.Stat(dst); err == nil && os.SameFile(srcSt, dstSt) {
			return fmt.Errorf("%s and %s are the same file", pyrepr.StrRepr(src), pyrepr.StrRepr(dst))
		}
	}
	for _, name := range []string{src, dst} {
		if st, err := os.Stat(name); err == nil && st.Mode()&fs.ModeNamedPipe != 0 {
			return fmt.Errorf("`%s` is a named pipe", name)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		if errors.Is(err, syscall.EISDIR) {
			if _, statErr := os.Lstat(dst); statErr != nil {
				return fmt.Errorf("Directory does not exist: %s", dst)
			}
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Copies access and modify times, extended attributes, permission bits and file flags like shutil copystat
func copyStat(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	atime, err := fileAccessTime(src, st)
	if err != nil {
		return err
	}
	if err := os.Chtimes(dst, atime, st.ModTime()); err != nil {
		return err
	}
	if err := copyXattrs(src, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, st.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	return copyFlags(dst, st)
}
