package tools

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Byte order mark a write target may start with
var utf8BOMBytes = []byte{0xef, 0xbb, 0xbf}

// Raised when a probed path exists but is not a regular file
var errNativeNotRegular = errors.New("not a regular file")

// Open file of a regular path, nil for a missing one, judged by fstat so a FIFO never blocks
func nativeOpenRegular(path string) (*os.File, error) {
	flags := os.O_RDONLY
	if runtime.GOOS != "windows" {
		flags |= syscall.O_NONBLOCK
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errNativeNotRegular
	}
	return f, nil
}

// Native write target probe of the BOM flag and the whole body or its first 4 KB sample
type nativeProbeResult struct {
	hasBOM bool
	body   []byte
}

// Probe of a write target, ok false when the shell has to answer instead
func nativeProbe(path string, wantText bool) (nativeProbeResult, bool) {
	f, err := nativeOpenRegular(path)
	if err != nil {
		return nativeProbeResult{}, false
	}
	if f == nil {
		return nativeProbeResult{}, true
	}
	defer f.Close()
	var data []byte
	if wantText {
		data, err = io.ReadAll(f)
	} else {
		buf := make([]byte, 4096)
		var n int
		n, err = io.ReadFull(f, buf)
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = nil
		}
		data = buf[:n]
	}
	if err != nil {
		return nativeProbeResult{}, false
	}
	return nativeProbeResult{hasBOM: bytes.HasPrefix(data, utf8BOMBytes), body: data}, true
}

// Atomic replace of a path through a temp in its directory, an error message on failure
func nativeAtomicWrite(path string, data []byte) string {
	target := path
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target = pyRealpath(path)
	}
	parent := posixDirname(target)
	if parent == "" {
		parent = "."
	}
	if err := pyMakedirs(parent); err != nil {
		return nativeOSErrorText(err, target)
	}
	var mode *os.FileMode
	if st, err := os.Stat(target); err == nil {
		m := st.Mode().Perm() | st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		mode = &m
	} else if !errors.Is(err, os.ErrNotExist) {
		return nativeOSErrorText(err, target)
	}
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return nativeOSErrorText(err, target)
	}
	tmp := pyOSPathJoin(parent, ".icebath-tmp."+hex.EncodeToString(token))
	perm := os.FileMode(0o666)
	if mode != nil {
		perm = 0o600
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return nativeOSErrorText(err, target)
	}
	fail := func(err error) string {
		f.Close()
		os.Remove(tmp)
		return nativeOSErrorText(err, target)
	}
	if mode != nil {
		f.Chmod(*mode)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nativeOSErrorText(err, target)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return nativeOSErrorText(err, target)
	}
	return ""
}

// Hex sha256 of a regular file, false when it cannot be read natively
func nativeSha256Of(path string) (string, bool) {
	f, err := nativeOpenRegular(path)
	if err != nil || f == nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// Python OSError text of strerror then the filename, the fallback name when the error carries none
func nativeOSErrorText(err error, fallback string) string {
	msg, name := nativeStrerror(err), ""
	var pe *os.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		name = pe.Path
	case errors.As(err, &le):
		name = le.Old
	}
	if name == "" {
		name = fallback
	}
	return msg + ": " + name
}

// C strerror text of the errno inside an error, the error text when there is none
func nativeStrerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := errno.Error()
		if runtime.GOOS != "windows" && text != "" {
			return strings.ToUpper(text[:1]) + text[1:]
		}
		return text
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// Recursive directory creation with the os.makedirs exist_ok semantics and error paths
func pyMakedirs(name string) error {
	head, tail := posixSplit(name)
	if tail == "" {
		head, tail = posixSplit(head)
	}
	if head != "" && tail != "" {
		if _, err := os.Stat(head); err != nil {
			if err := pyMakedirs(head); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if tail == "." {
				return nil
			}
		}
	}
	if err := os.Mkdir(name, 0o777); err != nil {
		if st, serr := os.Stat(name); serr == nil && st.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// posixpath.split keeping a root of only slashes intact
func posixSplit(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// posixpath.dirname
func posixDirname(p string) string {
	head, _ := posixSplit(p)
	return head
}

// posixpath.abspath against the process working directory
func posixAbspath(p string) string {
	if !strings.HasPrefix(p, "/") {
		if cwd, err := os.Getwd(); err == nil {
			p = posixJoin(filepath.ToSlash(cwd), p)
		}
	}
	return posixNormpath(p)
}

// Non strict os.path.realpath that follows dangling and looping links as far as they resolve
func pyRealpath(p string) string {
	path, _ := pyJoinRealpath("", p, map[string]*string{})
	return posixAbspath(path)
}

// One step of the realpath walk, false once a symlink loop stops resolution
func pyJoinRealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		name, after, _ := strings.Cut(rest, "/")
		rest = after
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path != "" {
				var last string
				path, last = posixSplit(path)
				if last == ".." {
					path = posixJoin(posixJoin(path, ".."), "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := posixJoin(path, name)
		st, err := os.Lstat(newpath)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if resolved, ok := seen[newpath]; ok {
			if resolved != nil {
				path = *resolved
				continue
			}
			return posixJoin(newpath, rest), false
		}
		seen[newpath] = nil
		link, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		var ok bool
		path, ok = pyJoinRealpath(path, link, seen)
		if !ok {
			return posixJoin(path, rest), false
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, true
}
