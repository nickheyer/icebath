//go:build windows

package pypath

import (
	"errors"
	"os"
	"strings"
	"syscall"

	winapi "golang.org/x/sys/windows"
)

// ntpath.abspath, GetFullPathNameW of the normalized path with the CPython join fallback when it fails
func ntAbspath(p string) (string, error) {
	if full, err := syscall.FullPath(NtNormpath(p)); err == nil {
		return full, nil
	}
	if !NtIsAbs(p) {
		drive, root, rest := NtSplitRoot(p)
		if drive != "" || root != "" {
			if full, err := syscall.FullPath(drive + root); err == nil {
				p = NtJoin(full, rest)
			} else {
				p = drive + `\` + rest
			}
		} else {
			cwd, err := Getcwd()
			if err != nil {
				return "", err
			}
			p = NtJoin(cwd, p)
		}
	}
	return NtNormpath(p), nil
}

// Windows error code an OSError would carry as winerror
func winerror(err error) syscall.Errno {
	var errno syscall.Errno
	errors.As(err, &errno)
	return errno
}

// Reports whether realpath under the strict mode swallows an OSError, FileNotFoundError alone when missing parts are allowed
func ntIgnored(err error, strict Strict) bool {
	switch strict {
	case Lax:
		return true
	case AllowMissing:
		return OSErrorClass(err) == "FileNotFoundError"
	}
	return false
}

func winerrorIn(err error, codes ...syscall.Errno) bool {
	code := winerror(err)
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

// GetFinalPathNameByHandleW flag for a drive letter path, the one CPython passes
const volumeNameDOS = 0

// nt._getfinalpathname, the GetFinalPathNameByHandleW name of the opened path
func getFinalPathName(p string) (string, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return "", &ValueError{Msg: "_getfinalpathname: embedded null character in path"}
	}
	name, err := winapi.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := winapi.CreateFile(name, 0, 0, nil, winapi.OPEN_EXISTING, winapi.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer winapi.CloseHandle(h)
	buf := make([]uint16, winapi.MAX_PATH)
	for {
		n, err := winapi.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return winapi.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}

// nt._findfirstfile, the stored name of the entry FindFirstFileW matches
func findFirstFile(p string) (string, error) {
	name, err := winapi.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	var data winapi.Win32finddata
	h, err := winapi.FindFirstFile(name, &data)
	if err != nil {
		return "", err
	}
	winapi.FindClose(h)
	return winapi.UTF16ToString(data.FileName[:]), nil
}

// ntpath._readlink_deep, following link targets until one repeats or the OS refuses with an allowed code
func ntReadlinkDeep(p string, strict Strict) (string, error) {
	seen := map[string]bool{}
	for !seen[NtNormcase(p)] {
		seen[NtNormcase(p)] = true
		if strings.IndexByte(p, 0) >= 0 {
			break
		}
		old := p
		target, err := os.Readlink(p)
		if err != nil {
			if ntIgnored(err, strict) && winerrorIn(err, 1, 2, 3, 5, 21, 32, 50, 67, 87, 4390, 4392, 4393) {
				break
			}
			return "", err
		}
		p = target
		if !NtIsAbs(p) {
			if st, err := os.Lstat(old); err != nil || st.Mode()&os.ModeSymlink == 0 {
				p = old
				break
			}
			p = NtNormpath(NtJoin(NtDirname(old), p))
		}
	}
	return p, nil
}

// ntpath._getfinalpathname_nonstrict, resolving the longest openable prefix and appending the rest
func ntFinalPathNonstrict(p string, strict Strict) (string, error) {
	join := func(path, tail string) string {
		if strings.HasSuffix(path, `\`) || tail == "" {
			return path + tail
		}
		return path + `\` + tail
	}
	tail := ""
	for p != "" {
		final, err := getFinalPathName(p)
		if err == nil {
			return join(final, tail), nil
		}
		if !ntIgnored(err, strict) || !winerrorIn(err, 1, 2, 3, 5, 21, 32, 50, 53, 65, 67, 87, 123, 161, 1005, 1920, 1921) {
			return "", err
		}
		newPath, rerr := ntReadlinkDeep(p, strict)
		if rerr == nil && newPath != p {
			return join(newPath, tail), nil
		} else if rerr != nil && !ntIgnored(rerr, strict) {
			return "", rerr
		}
		var name string
		if winerrorIn(err, 1, 5, 32, 50, 87, 1920, 1921) {
			found, ferr := findFirstFile(p)
			if ferr == nil {
				name = found
				p, _ = NtSplit(p)
			} else if ntIgnored(ferr, strict) {
				p, name = NtSplit(p)
			} else {
				return "", ferr
			}
		} else {
			p, name = NtSplit(p)
		}
		if p != "" && name == "" {
			return p + tail, nil
		}
		tail = join(name, tail)
	}
	return tail, nil
}

// ntpath.realpath of CPython 3.14, final path names with the extended prefix dropped when it names the same file
func ntRealpath(p string, strict Strict) (string, error) {
	const prefix, uncPrefix = `\\?\`, `\\?\UNC\`
	p = NtNormpath(p)
	if NtNormcase(p) == "nul" {
		return `\\.\NUL`, nil
	}
	hadPrefix := strings.HasPrefix(p, prefix)
	if !hadPrefix && !NtIsAbs(p) {
		abs, err := ntAbspath(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	var initial syscall.Errno
	final, err := getFinalPathName(p)
	var valueErr *ValueError
	switch {
	case err == nil:
		p = final
	case errors.As(err, &valueErr):
		if strict != Lax {
			return "", errors.New(valueErr.Msg)
		}
		p = NtNormpath(p)
	case ntIgnored(err, strict):
		initial = winerror(err)
		if p, err = ntFinalPathNonstrict(p, strict); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	if !hadPrefix && strings.HasPrefix(p, prefix) {
		spath := ""
		if strings.HasPrefix(p, uncPrefix) {
			spath = `\\` + p[len(uncPrefix):]
		} else if len(p) > len(prefix)+1 && strings.HasPrefix(p[len(prefix)+1:], `:\`) {
			spath = p[len(prefix):]
		}
		if spath != "" {
			again, err := getFinalPathName(spath)
			if err == nil && again == p {
				p = spath
			} else if err != nil && !errors.As(err, &valueErr) && winerror(err) == initial {
				p = spath
			}
		}
	}
	return p, nil
}
