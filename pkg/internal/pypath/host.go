package pypath

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

var windows = runtime.GOOS == "windows"

// os.path.normpath of the host
func Normpath(p string) string {
	if windows {
		return NtNormpath(p)
	}
	return PosixNormpath(p)
}

// os.path.join of the host
func Join(a string, p ...string) string {
	if windows {
		return NtJoin(a, p...)
	}
	return PosixJoin(a, p...)
}

// os.path.splitroot of the host
func SplitRoot(p string) (string, string, string) {
	if windows {
		return NtSplitRoot(p)
	}
	return PosixSplitRoot(p)
}

// os.path.split of the host
func Split(p string) (string, string) {
	if windows {
		return NtSplit(p)
	}
	return PosixSplit(p)
}

// os.path.basename of the host
func Basename(p string) string {
	_, tail := Split(p)
	return tail
}

// os.path.dirname of the host
func Dirname(p string) string {
	head, _ := Split(p)
	return head
}

// os.path.splitext of the host
func Splitext(p string) (string, string) {
	if windows {
		return NtSplitext(p)
	}
	return PosixSplitext(p)
}

// os.path.isabs of the host
func IsAbs(p string) bool {
	if windows {
		return NtIsAbs(p)
	}
	return PosixIsAbs(p)
}

// os.path.expanduser of the host
func Expanduser(p string) string {
	if windows {
		return NtExpanduser(p)
	}
	return PosixExpanduser(p)
}

// posixpath.expanduser from HOME or the password database, a named user looked up by name
func PosixExpanduser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	i := strings.IndexByte(p[1:], '/') + 1
	if i == 0 {
		i = len(p)
	}
	var home string
	if i == 1 {
		if h, ok := os.LookupEnv("HOME"); ok {
			home = h
		} else if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			home = u.HomeDir
		} else {
			return p
		}
	} else {
		u, err := user.Lookup(p[1:i])
		if err != nil {
			return p
		}
		home = u.HomeDir
	}
	if out := strings.TrimRight(home, "/") + p[i:]; out != "" {
		return out
	}
	return "/"
}

// os.getcwd, failing with the bare errno CPython raises without a filename
func Getcwd() (string, error) {
	cwd, err := os.Getwd()
	if err == nil {
		return cwd, nil
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "", errno
	}
	return "", err
}

// os.path.abspath, a relative path joined to the working directory whose getcwd failure is returned
func Abspath(p string) (string, error) {
	if windows {
		return ntAbspath(p)
	}
	if !PosixIsAbs(p) {
		cwd, err := Getcwd()
		if err != nil {
			return "", err
		}
		p = PosixJoin(cwd, p)
	}
	return PosixNormpath(p), nil
}

// Strict mode of os.path.realpath
type Strict int

const (
	// Unresolvable components kept literally
	Lax Strict = iota
	// Every OSError returned
	StrictAll
	// Missing components kept literally and every other OSError returned
	AllowMissing
)

// os.path.realpath with symlinks resolved, loops kept literal unless strict
func Realpath(p string, strict Strict) (string, error) {
	if windows {
		return ntRealpath(p, strict)
	}
	return posixRealpath(p, strict)
}

func ignoredRealpathErr(err error, strict Strict) bool {
	switch strict {
	case Lax:
		return true
	case AllowMissing:
		return errors.Is(err, fs.ErrNotExist)
	}
	return false
}

// posixpath.realpath walking components with a seen map exactly like CPython 3.14
func posixRealpath(filename string, strict Strict) (string, error) {
	type item struct {
		name   string
		marker bool
	}
	parts := strings.Split(filename, "/")
	rest := make([]item, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		rest = append(rest, item{name: parts[i]})
	}
	partCount := len(parts)
	path := "/"
	if !strings.HasPrefix(filename, "/") {
		cwd, err := Getcwd()
		if err != nil {
			return "", err
		}
		path = cwd
	}
	seen := map[string]*string{}
	pop := func() item {
		it := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		return it
	}
	for partCount > 0 {
		it := pop()
		if it.marker {
			resolved := path
			seen[pop().name] = &resolved
			continue
		}
		partCount--
		name := it.name
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path = path[:strings.LastIndexByte(path, '/')]; path == "" {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + name
		if path == "/" {
			newpath = path + name
		}
		target, err := realpathStep(newpath, strict, partCount, seen)
		switch {
		case err == errKeepNewpath:
			path = newpath
			continue
		case err == errUseSeen:
			path = *seen[newpath]
			continue
		case err != nil:
			if !ignoredRealpathErr(err, strict) {
				return "", err
			}
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		rest = append(rest, item{name: newpath}, item{marker: true})
		tparts := strings.Split(target, "/")
		for i := len(tparts) - 1; i >= 0; i-- {
			rest = append(rest, item{name: tparts[i]})
		}
		partCount += len(tparts)
	}
	return path, nil
}

var (
	errKeepNewpath = errors.New("keep newpath")
	errUseSeen     = errors.New("use seen")
)

// One lstat and readlink step of realpath, the link target or a control error
func realpathStep(newpath string, strict Strict, partCount int, seen map[string]*string) (string, error) {
	st, err := os.Lstat(newpath)
	if err != nil {
		return "", err
	}
	if st.Mode()&fs.ModeSymlink == 0 {
		if strict != Lax && partCount > 0 && !st.IsDir() {
			return "", &fs.PathError{Op: "realpath", Path: newpath, Err: syscall.ENOTDIR}
		}
		return "", errKeepNewpath
	}
	if prior, ok := seen[newpath]; ok {
		if prior != nil {
			return "", errUseSeen
		}
		if strict != Lax {
			return "", &fs.PathError{Op: "realpath", Path: newpath, Err: syscall.ELOOP}
		}
		return "", errKeepNewpath
	}
	return os.Readlink(newpath)
}
