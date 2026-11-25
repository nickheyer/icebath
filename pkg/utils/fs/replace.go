package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"runtime"
	"time"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Flushes a file to disk; every atomic write runs it before the rename, and tests may replace it to simulate a failing disk
var SyncFile = func(f *os.File) error { return f.Sync() }

// Flushes a directory entry to disk, skipped on Windows and on any error; durable writes run it after the rename, and tests may replace it to observe them
var SyncDir = syncDir

func syncDir(path string) {
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

// Temp file named prefix, random characters and suffix in the directory Replace publishes target into, the directory of the link itself when that one refuses it
func CreateTempBeside(target, prefix, suffix string) (*os.File, error) {
	linkDir := pathutil.DirKeepDotDot(target)
	stageDir := pathutil.DirKeepDotDot(publishPath(target))
	f, err := os.CreateTemp(stageDir, prefix+"*"+suffix)
	if err != nil && errors.Is(err, fs.ErrPermission) && stageDir != linkDir {
		return os.CreateTemp(linkDir, prefix+"*"+suffix)
	}
	return f, err
}

// Renames tmp over target, or over the file a symlinked target points at so the link stays, copying across devices and retrying a target another Windows process holds; gives back the path written
func Replace(tmp, target string) (string, error) {
	publish := publishPath(target)
	return publish, replaceFile(tmp, publish)
}

// File a symlinked target points at, resolved as far as it exists, else the target itself
func publishPath(target string) string {
	st, err := os.Lstat(target)
	if err != nil || st.Mode()&fs.ModeSymlink == 0 {
		return target
	}
	if resolved, err := pathutil.Resolve(target); err == nil {
		return resolved
	}
	return target
}

func replaceFile(tmp, target string) error {
	err := os.Rename(tmp, target)
	if err == nil {
		return nil
	}
	contended := IsRenameContended(err)
	if !contended && !IsRenameCrossDevice(err) {
		return err
	}
	if contended {
		for attempt := range 4 {
			time.Sleep(replaceBackoff(attempt))
			err = os.Rename(tmp, target)
			if err == nil {
				return nil
			}
			if IsRenameCrossDevice(err) {
				contended = false
				break
			}
			if !IsRenameContended(err) {
				return err
			}
		}
	}
	if contended {
		return rewriteInPlace(tmp, target)
	}
	return copyOver(tmp, target)
}

// Wait before retry n of a contended rename: 20ms doubling up to 100ms, plus up to half again of random jitter
func replaceBackoff(n int) time.Duration {
	d := min(20*time.Millisecond<<n, 100*time.Millisecond)
	return d + rand.N(d/2+1)
}

// Overwrites a target another process holds open through a handle of its own, the last resort when renaming over it keeps failing; tmp is removed once the content is in, a failure to remove it no longer failing the publish
func rewriteInPlace(tmp, target string) error {
	in, err := os.Open(tmp)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		in.Close()
		return err
	}
	n, err := io.Copy(out, in)
	in.Close()
	if err == nil {
		err = out.Truncate(n)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_ = os.Remove(tmp)
	return nil
}

// Copies tmp onto a target on another device or a busy mount point, flushed; the target is created owner only and given tmp's permission bits before any byte lands, so the content is never more exposed than its final mode allows, then it gets tmp's modification time and tmp is removed, a failure to remove it no longer failing the publish
func copyOver(tmp, target string) error {
	in, err := os.Open(tmp)
	if err != nil {
		return err
	}
	st, err := in.Stat()
	if err != nil {
		in.Close()
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		in.Close()
		return err
	}
	// Best effort, as a bind mounted target owned by another user refuses it while still taking the content
	_ = out.Chmod(st.Mode().Perm())
	_, err = io.Copy(out, in)
	in.Close()
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_ = os.Chtimes(target, st.ModTime(), st.ModTime())
	_ = os.Remove(tmp)
	return nil
}
