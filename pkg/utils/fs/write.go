package fsutil

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/home"
)

// Temp file name prefix of a write whose WriteOptions leave Prefix empty
const defaultPrefix = ".tmp_"

var errPendingDone = errors.New("pending file already committed or cleaned up")

// Settings of an atomic write: Prefix starts the temp file name (".tmp_" when empty); Mode forces the permission bits, setuid, setgid and sticky included, else the replaced file keeps its bits and a new file gets CreateMode or the umask default; KeepOwner hands the file back to the replaced file's owner; NativeLineEnds makes WriteFile write \n as \r\n on Windows; SyncDir flushes the directory after the rename
type WriteOptions struct {
	Prefix         string
	Mode           *fs.FileMode
	CreateMode     *fs.FileMode
	KeepOwner      bool
	NativeLineEnds bool
	SyncDir        bool
}

// Writes data to path through a temp file beside the file path finally names, flushed and renamed over it so readers see the old content or the new and never a mix; the parent directory is made first
func WriteFile(ctx context.Context, path string, data []byte, o WriteOptions) error {
	p, err := NewPendingFile(ctx, path, o)
	if err != nil {
		return err
	}
	defer p.Cleanup()
	if o.NativeLineEnds && runtime.GOOS == "windows" {
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	}
	if _, err := p.Write(data); err != nil {
		return err
	}
	return p.Commit()
}

// Temp file of a streamed atomic write, written through the embedded *os.File and published over its target by Commit; it must not be closed directly
type PendingFile struct {
	*os.File
	target  string
	mode    uint32
	modeSet bool
	owner   *Owner
	syncDir bool
	done    bool
}

// Temp file for an atomic write of path under o, staged where Replace publishes and already carrying the final permission bits; the parent directory is made first, refusing a deleted profile home; publish it with Commit and defer Cleanup
func NewPendingFile(ctx context.Context, path string, o WriteOptions) (*PendingFile, error) {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return nil, err
	}
	mode, modeSet := writeMode(path, o)
	var owner *Owner
	if o.KeepOwner {
		owner = OwnerOf(path)
	}
	prefix := o.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	f, err := CreateTempBeside(path, prefix, ".tmp")
	if err != nil {
		return nil, err
	}
	if modeSet {
		if err := fchmodStaged(f, mode); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, err
		}
	}
	return &PendingFile{File: f, target: path, mode: mode, modeSet: modeSet, owner: owner, syncDir: o.SyncDir}, nil
}

// Permission bits the published file gets: forced, else those of the file it replaces, else the create mode or the umask default; false where the platform sets none
func writeMode(path string, o WriteOptions) (uint32, bool) {
	if o.Mode != nil {
		return posixBits(*o.Mode), true
	}
	if bits, ok := PermBits(path); ok {
		return bits, true
	}
	if o.CreateMode != nil {
		return posixBits(*o.CreateMode), true
	}
	return newFileMode()
}

// Flushes and closes the temp file and renames it over the target, over the file a symlinked target points at so the link stays, then puts the captured owner and permission bits back and flushes the directory when asked; the temp file is gone whatever the outcome, a panic on the way included
func (p *PendingFile) Commit() error {
	if p.done {
		return errPendingDone
	}
	p.done = true
	tmp := p.Name()
	replaced := false
	defer func() {
		if !replaced {
			p.File.Close()
			os.Remove(tmp)
		}
	}()
	if err := SyncFile(p.File); err != nil {
		return err
	}
	if err := p.File.Close(); err != nil {
		return err
	}
	published, err := Replace(tmp, p.target)
	if err != nil {
		return err
	}
	replaced = true
	// The owner goes back first since a chown clears setuid and setgid
	RestoreOwner(published, p.owner)
	if p.modeSet {
		_ = chmodBits(published, p.mode)
	}
	if p.syncDir {
		SyncDir(filepath.Dir(published))
	}
	return nil
}

// Closes and removes the temp file unless Commit already published it; safe to call any number of times
func (p *PendingFile) Cleanup() error {
	if p.done {
		return nil
	}
	p.done = true
	cerr := p.File.Close()
	if rerr := os.Remove(p.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return rerr
	}
	return cerr
}
