package pyfs

import (
	"context"
	"errors"
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

// Writes text through a temp file beside the published target, fsync, and an atomic rename
func AtomicWriteText(ctx context.Context, path, content, prefix string) error {
	return atomicWriteText(ctx, path, content, prefix, nil, false)
}

// Writes like AtomicWriteText with the permission bits forced and the existing owner carried across
func AtomicWriteTextMode(ctx context.Context, path, content, prefix string, mode os.FileMode) error {
	return atomicWriteText(ctx, path, content, prefix, &mode, true)
}

func atomicWriteText(ctx context.Context, path, content, prefix string, forced *os.FileMode, preserveOwner bool) error {
	if !utf8.ValidString(content) {
		return unicodeEncodeError(content)
	}
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	var mode uint32
	modeSet := false
	if forced != nil {
		mode, modeSet = posixModeBits(*forced), true
	} else if _, err := os.Stat(path); err != nil {
		var fresh os.FileMode
		fresh, modeSet = defaultNewFileMode()
		mode = posixModeBits(fresh)
	}
	var owner *FileOwner
	if preserveOwner {
		owner = PreserveFileOwner(path)
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
	if modeSet {
		if err := fchmodStaged(tmp, mode); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.WriteString(platformNewlines(content)); err != nil {
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
	restoreFileMetadata(publish, owner, mode, modeSet)
	return nil
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
