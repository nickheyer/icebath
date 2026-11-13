package pyfs

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
)

// UnicodeEncodeError Python raises writing text with lone surrogates as utf-8
type EncodeError struct{ Msg string }

func (e *EncodeError) Error() string { return e.Msg }

// File flush every atomic writer runs before the replace, the os.fsync hermes tests fail
var Fsync = func(f *os.File) error { return f.Sync() }

// Keyword arguments of the shared atomic writer, Mode applying only when ModeSet
type WriteOptions struct {
	Prefix        string
	Mode          uint32
	ModeSet       bool
	PreserveOwner bool
	FsyncDir      bool
}

// Temp file beside the target, fsync and atomic replace with owner and mode reapplied, the temp removed on failure
func AtomicWrite(ctx context.Context, path string, write func(*os.File) error, o WriteOptions) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	mode, modeSet := o.Mode, o.ModeSet
	if !modeSet {
		if _, err := os.Stat(path); err != nil {
			var fresh os.FileMode
			fresh, modeSet = defaultNewFileMode()
			mode = PosixModeBits(fresh)
		}
	}
	var owner *FileOwner
	if o.PreserveOwner {
		owner = PreserveFileOwner(path)
	}
	tmp, err := MkstempBeside(path, o.Prefix, ".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if modeSet {
		if err := fchmodStaged(tmp, mode); err != nil {
			return fail(err)
		}
	}
	if err := write(tmp); err != nil {
		return fail(err)
	}
	if err := Fsync(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	replaced, err := AtomicReplace(tmpName, path)
	if err != nil {
		os.Remove(tmpName)
		return err
	}
	restoreFileMetadata(replaced, owner, mode, modeSet)
	if o.FsyncDir {
		FsyncDirectory(filepath.Dir(replaced))
	}
	return nil
}

// Existing permission bits of path when preserving, else the create mode for a file that does not exist
func ModeForWrite(path string, createMode uint32, createSet, preserve bool) (uint32, bool) {
	if preserve {
		if mode, ok := PreserveFileMode(path); ok {
			return mode, true
		}
	}
	if _, err := os.Stat(path); err == nil {
		return 0, false
	}
	return createMode, createSet
}

// Keyword arguments of an atomic text write, Mode forcing the bits and CreateMode applying to a new file only
type TextWrite struct {
	Prefix       string
	PreserveMode bool
	CreateMode   *os.FileMode
	Mode         *os.FileMode
	FsyncDir     bool
}

// Writes utf-8 text through the shared atomic writer under the given keyword arguments
func AtomicWriteTextWith(ctx context.Context, path, content string, o TextWrite) error {
	prefix := o.Prefix
	if prefix == "" {
		prefix = ".tmp_"
	}
	var mode uint32
	var modeSet bool
	if o.Mode != nil {
		mode, modeSet = PosixModeBits(*o.Mode), true
	} else {
		var create uint32
		if o.CreateMode != nil {
			create = PosixModeBits(*o.CreateMode)
		}
		mode, modeSet = ModeForWrite(path, create, o.CreateMode != nil, o.PreserveMode)
	}
	write := func(f *os.File) error {
		if !utf8.ValidString(content) {
			return unicodeEncodeError(content)
		}
		_, err := f.WriteString(PlatformNewlines(content))
		return err
	}
	return AtomicWrite(ctx, path, write, WriteOptions{Prefix: prefix, Mode: mode, ModeSet: modeSet, PreserveOwner: o.PreserveMode, FsyncDir: o.FsyncDir})
}

// Writes text through a temp file beside the published target, fsync, and an atomic rename
func AtomicWriteText(ctx context.Context, path, content, prefix string) error {
	return AtomicWriteTextWith(ctx, path, content, TextWrite{Prefix: prefix})
}

// Writes like AtomicWriteText with the permission bits forced
func AtomicWriteTextMode(ctx context.Context, path, content, prefix string, mode os.FileMode) error {
	return AtomicWriteTextWith(ctx, path, content, TextWrite{Prefix: prefix, Mode: &mode})
}

// Real file a symlink points at, else the target itself
func publishPath(target string) string {
	st, err := os.Lstat(target)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return target
	}
	return realpathNonStrict(target)
}

// Resolves every symlink it can and keeps the unresolved remainder like a non strict realpath
func realpathNonStrict(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	cur := abs
	for hops := 0; hops < 40; hops++ {
		st, err := os.Lstat(cur)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			break
		}
		link, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(cur), link)
		}
		cur = filepath.Clean(link)
	}
	dir, base := filepath.Split(cur)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolved, base)
	}
	return cur
}

// Renames tmp onto target, copying across devices and retrying a contended Windows rename
func atomicReplace(tmp, target string) error {
	err := os.Rename(tmp, target)
	if err == nil {
		return nil
	}
	contended := IsContendedReplaceError(err)
	if !IsCrossDeviceError(err) && !contended {
		return err
	}
	if contended {
		for attempt := 1; attempt <= 4; attempt++ {
			time.Sleep(jitteredBackoff(attempt, 0.02, 0.1))
			retryErr := os.Rename(tmp, target)
			if retryErr == nil {
				return nil
			}
			if IsCrossDeviceError(retryErr) {
				contended = false
				break
			}
			if !IsContendedReplaceError(retryErr) {
				return retryErr
			}
		}
	}
	if contended {
		return rewriteInPlace(tmp, target)
	}
	return copyFallback(tmp, target)
}

// Overwrites the target through its existing handle as the last resort for a held file
func rewriteInPlace(tmp, target string) error {
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		f.Close()
		return err
	}
	f.Sync()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(tmp)
}

// Copies tmp onto target with its mode, fsyncs and unlinks tmp for cross device renames
func copyFallback(tmp, target string) error {
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(tmp); err == nil {
		os.Chmod(target, st.Mode().Perm())
		os.Chtimes(target, st.ModTime(), st.ModTime())
	}
	if rf, err := os.Open(target); err == nil {
		rf.Sync()
		rf.Close()
	}
	return os.Remove(tmp)
}

// Exponential delay capped at the max plus up to half again of uniform jitter
func jitteredBackoff(attempt int, base, maxDelay float64) time.Duration {
	exponent := max(0, attempt-1)
	delay := maxDelay
	if exponent < 63 && base > 0 {
		delay = math.Min(base*math.Pow(2, float64(exponent)), maxDelay)
	}
	return time.Duration((delay + rand.Float64()*0.5*delay) * float64(time.Second))
}

// Python UnicodeEncodeError text for the first lone surrogate in WTF-8 text
func unicodeEncodeError(s string) error {
	pos := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if i+2 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 {
				sur := rune(s[i]&0x0f)<<12 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f)
				return &EncodeError{Msg: fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", sur, pos)}
			}
			return &EncodeError{Msg: fmt.Sprintf("'utf-8' codec can't encode character '\\x%02x' in position %d: surrogates not allowed", s[i], pos)}
		}
		i += size
		pos++
	}
	return &EncodeError{Msg: "'utf-8' codec can't encode characters: surrogates not allowed"}
}
