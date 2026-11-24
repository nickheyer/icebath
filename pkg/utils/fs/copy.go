package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Error a copy gives when source and destination name the same file
var ErrSameFile = errors.New("source and destination are the same file")

var errSpecialFile = errors.New("not a regular file")

// Copies the content of src to dst, or into dst when that is a directory, then its permission bits with setuid, setgid and sticky, both times and on Linux its extended attributes, symlinks followed; a copy onto itself fails with ErrSameFile, a directory, fifo, socket or device src fails before dst is touched
func CopyFile(src, dst string) error {
	dst = intoDir(src, dst)
	if err := copyContent(src, dst); err != nil {
		return err
	}
	return CopyMetadata(src, dst)
}

// Copies the permission bits with setuid, setgid and sticky, both times and on Linux the extended attributes of src onto dst, symlinks followed
func CopyMetadata(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyMetadataFrom(src, dst, fi)
}

// CopyFile taking the times and mode from a stat of src made before its content was read, as a tree copy has one
func copyFileFrom(src, dst string, fi fs.FileInfo) error {
	dst = intoDir(src, dst)
	if err := copyContent(src, dst); err != nil {
		return err
	}
	return copyMetadataFrom(src, dst, fi)
}

// Destination of a copy, the source's name inside dst when dst is a directory
func intoDir(src, dst string) string {
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		return filepath.Join(dst, filepath.Base(src))
	}
	return dst
}

// Copies the bytes of src into dst, refusing before dst is touched a src that is a directory, a fifo, a socket or a device, whose reading would fail, block or never end, and a dst that is src itself
func copyContent(src, dst string) error {
	a, err := os.Stat(src)
	if err != nil {
		return err
	}
	if a.IsDir() {
		return &fs.PathError{Op: "copy", Path: src, Err: syscall.EISDIR}
	}
	if a.Mode()&(fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice) != 0 {
		return &fs.PathError{Op: "copy", Path: src, Err: errSpecialFile}
	}
	if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
		return &os.LinkError{Op: "copy", Old: src, New: dst, Err: ErrSameFile}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyMetadataFrom(src, dst string, fi fs.FileInfo) error {
	if err := os.Chtimes(dst, Atime(fi), fi.ModTime()); err != nil {
		return err
	}
	if err := copyXattr(src, dst); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}
