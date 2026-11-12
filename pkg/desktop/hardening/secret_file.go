package hardening

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
)

// Owner only mode of credential files
const SecretFileMode = 0o600

// Stat facts the secret file guards read
type FileStat struct {
	IsFile      bool
	IsSymlink   bool
	Mode        fs.FileMode
	UID         int
	HasOwnerUID bool
}

// File system calls of the secret file helpers
type FS interface {
	Lstat(path string) (FileStat, error)
	Chmod(path string, mode fs.FileMode) error
	RemoveForce(path string) error
	WriteFile(path string, data []byte, mode fs.FileMode) error
	Rename(from, to string) error
}

// Secret file options, the real file system and host platform when left empty
type SecretFileOptions struct {
	FS       FS
	Platform string
	UID      func() (int, bool)
}

type osFS struct{}

func (osFS) Lstat(path string) (FileStat, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return FileStat{}, err
	}
	uid, ok := ownerUID(st)
	return FileStat{IsFile: st.Mode().IsRegular(), IsSymlink: st.Mode()&fs.ModeSymlink != 0, Mode: st.Mode().Perm(), UID: uid, HasOwnerUID: ok}, nil
}
func (osFS) Chmod(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
func (osFS) RemoveForce(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
func (osFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	return os.WriteFile(path, data, mode)
}
func (osFS) Rename(from, to string) error { return os.Rename(from, to) }

// Real file system of the secret file helpers
var OSFS FS = osFS{}

func (o SecretFileOptions) resolved() SecretFileOptions {
	if o.FS == nil {
		o.FS = OSFS
	}
	if o.Platform == "" {
		o.Platform = runtime.GOOS
		if o.Platform == "windows" {
			o.Platform = "win32"
		}
	}
	if o.UID == nil {
		o.UID = processUID
	}
	return o
}

// Tightens an owned regular file to owner only, never a symlink or another user's file, true when it now is
func TightenSecretFileMode(path string, o SecretFileOptions) bool {
	o = o.resolved()
	if o.Platform == "win32" {
		return true
	}
	st, err := o.FS.Lstat(path)
	if err != nil || !st.IsFile || st.IsSymlink {
		return false
	}
	if uid, ok := o.UID(); ok && (!st.HasOwnerUID || st.UID != uid) {
		return false
	}
	if st.Mode&0o777 == SecretFileMode {
		return true
	}
	if o.FS.Chmod(path, SecretFileMode) != nil {
		return false
	}
	return true
}

// Writes a credential file through a fresh owner only temporary renamed over the target
func WriteSecretFileAtomic(target string, data []byte, o SecretFileOptions) error {
	o = o.resolved()
	tmp := target + ".tmp"
	if err := o.FS.RemoveForce(tmp); err != nil {
		return err
	}
	if err := o.FS.WriteFile(tmp, data, SecretFileMode); err != nil {
		return err
	}
	TightenSecretFileMode(tmp, o)
	return o.FS.Rename(tmp, target)
}
