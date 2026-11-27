// File system steps every part of icebath takes the same way: lock files, whole file writes, copies, and the questions asked of a path
package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Writes data to path whole or not at all, through a temporary file beside it that is flushed and renamed over it, so a reader sees the
// old bytes or the new and a crash leaves one of them; the file gets perm, and its folder, made when missing, may be entered by whoever
// may read the file
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, perm)
}

// Write that leaves an existing file its permission bits, perm going to a new one
func Replace(path string, data []byte, perm os.FileMode) error {
	mode := perm
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return write(path, data, mode, perm)
}

func write(path string, data []byte, perm, folder os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, folder.Perm()|folder.Perm()&0o444>>2); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), perm); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err := Retry(func() error { return os.Rename(f.Name(), path) }); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// Runs a rename or removal again for about three seconds while another program, usually an antivirus scan on Windows, holds the file
func Retry(operation func() error) error {
	delay := 100 * time.Millisecond
	for i := 0; ; i++ {
		err := operation()
		if err == nil || i >= 5 || !Held(err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// Copies a file's bytes, permission bits and modification time over dst
func Copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, time.Now(), st.ModTime())
}

// Whether anything is at the path, through links
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Whether anything is at the path, a dangling link included
func Lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Whether the path is a folder, through links
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Whether the path is a regular file, through links
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Path with a leading ~ standing for the user's home folder expanded, left as it is when there is no home
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !(runtime.GOOS == "windows" && strings.HasPrefix(path, `~\`)) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

// Whether path is root or lies under it, both cleaned first
func Within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

// Hex sha256 of a file's bytes
func Digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Closes every file, what a close says left unsaid, as the ends of pipes a started child no longer needs
func CloseAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}
