package file

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const tempPrefix = ".icebath-tmp."

var errNotRegular = errors.New("not a regular file")

func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

func readRegular(path string) ([]byte, bool) {
	f, err := openRegular(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return data, true
}

type nativeTarget struct {
	hasBOM bool
	body   []byte
}

func probeNative(path string, wantText bool) (nativeTarget, bool) {
	f, err := openRegular(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nativeTarget{}, true
	}
	if err != nil {
		return nativeTarget{}, false
	}
	defer f.Close()
	var data []byte
	if wantText {
		data, err = io.ReadAll(f)
	} else {
		data, err = io.ReadAll(io.LimitReader(f, 4096))
	}
	if err != nil {
		return nativeTarget{}, false
	}
	return nativeTarget{hasBOM: len(data) >= len(bom) && string(data[:len(bom)]) == bom, body: data}, true
}

func sha256Native(path string) (string, bool) {
	f, err := openRegular(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(digest.Sum(nil)), true
}

func osErrorText(err error, fallback string) string {
	name := fallback
	inner := err
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		inner = pathErr.Err
		if pathErr.Path != "" {
			name = pathErr.Path
		}
	case errors.As(err, &linkErr):
		inner = linkErr.Err
		if linkErr.Old != "" {
			name = linkErr.Old
		}
	}
	msg := inner.Error()
	var errno syscall.Errno
	if errors.As(inner, &errno) {
		msg = errno.Error()
		if r, size := utf8.DecodeRuneInString(msg); size > 0 {
			msg = string(unicode.ToUpper(r)) + msg[size:]
		}
	}
	return msg + ": " + name
}

func resolveLinks(path string) string {
	if !strings.HasPrefix(path, "/") {
		if wd, err := os.Getwd(); err == nil {
			path = pathStyle{}.join(wd, path)
		}
	}
	resolved := "/"
	rest := strings.Split(path, "/")
	hops := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = pathStyle{}.dir(resolved)
			if resolved == "" {
				resolved = "/"
			}
			continue
		}
		next := pathStyle{}.join(resolved, name)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		hops++
		if err != nil || hops > 40 {
			resolved = next
			continue
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved
}

func makeDirs(name string) error {
	head, tail := pathStyle{}.split(name)
	if tail == "" {
		head, tail = pathStyle{}.split(head)
	}
	if head != "" && tail != "" {
		if _, err := os.Stat(head); err != nil {
			if err := makeDirs(head); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if tail == "." {
				return nil
			}
		}
	}
	if err := os.Mkdir(name, 0o777); err != nil {
		if fi, statErr := os.Stat(name); statErr == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

func atomicWriteNative(path string, data []byte) string {
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target = resolveLinks(path)
	}
	parent := pathStyle{}.dir(target)
	if parent == "" {
		parent = "."
	}
	if err := makeDirs(parent); err != nil {
		return osErrorText(err, target)
	}
	existing := false
	var mode os.FileMode
	fi, err := os.Stat(target)
	switch {
	case err == nil:
		existing = true
		mode = fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	case !errors.Is(err, fs.ErrNotExist):
		return osErrorText(err, target)
	}
	var token [6]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err.Error() + ": " + target
	}
	tmp := pathStyle{}.join(parent, tempPrefix+hex.EncodeToString(token[:]))
	perm := os.FileMode(0o666)
	if existing {
		perm = 0o600
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return osErrorText(err, target)
	}
	if existing {
		_ = f.Chmod(mode)
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return osErrorText(err, target)
	}
	return ""
}
