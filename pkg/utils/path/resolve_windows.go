package pathutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Errors from opening a path that leave a lax walk to try the part of the path above it
var finalPathTolerated = []syscall.Errno{
	windows.ERROR_INVALID_FUNCTION, windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_ACCESS_DENIED,
	windows.ERROR_NOT_READY, windows.ERROR_SHARING_VIOLATION, windows.ERROR_NOT_SUPPORTED, windows.ERROR_BAD_NETPATH,
	windows.ERROR_NETWORK_ACCESS_DENIED, windows.ERROR_BAD_NET_NAME, windows.ERROR_INVALID_PARAMETER, windows.ERROR_INVALID_NAME,
	windows.ERROR_BAD_PATHNAME, windows.ERROR_UNRECOGNIZED_VOLUME, windows.ERROR_CANT_ACCESS_FILE, windows.ERROR_CANT_RESOLVE_FILENAME,
}

// Errors from opening a path after which its directory entry is still worth reading for the stored name
var finalPathListable = []syscall.Errno{
	windows.ERROR_INVALID_FUNCTION, windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_NOT_SUPPORTED,
	windows.ERROR_INVALID_PARAMETER, windows.ERROR_CANT_ACCESS_FILE, windows.ERROR_CANT_RESOLVE_FILENAME,
}

// Errors from reading a link that end the chain of links at the path reached
var readlinkTolerated = []syscall.Errno{
	windows.ERROR_INVALID_FUNCTION, windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_ACCESS_DENIED,
	windows.ERROR_NOT_READY, windows.ERROR_SHARING_VIOLATION, windows.ERROR_NOT_SUPPORTED, windows.ERROR_BAD_NET_NAME,
	windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_A_REPARSE_POINT, windows.ERROR_INVALID_REPARSE_DATA,
	windows.ERROR_REPARSE_TAG_INVALID,
}

// GetFinalPathNameByHandleW flag asking for a drive letter path
const volumeNameDOS = 0

func errnoIn(err error, codes []syscall.Errno) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && slices.Contains(codes, errno)
}

// The target joined onto the resolved dir, the system applying a relative target's dot-dot to the directory the link really sits in, then resolved in turn
func resolveLink(dir, target string) (string, error) {
	base, err := resolve(dir, false)
	if err != nil {
		return "", err
	}
	return resolve(WindowsJoin(base, target), false)
}

// Final path of p from GetFinalPathNameByHandleW, with the \\?\ prefix dropped again when the short form names the same file
func resolve(p string, strict bool) (string, error) {
	const prefix, uncPrefix = `\\?\`, `\\?\UNC\`
	if strings.IndexByte(p, 0) >= 0 {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: syscall.EINVAL}
	}
	p = WindowsClean(p)
	if WindowsFoldCase(p) == "nul" {
		return `\\.\NUL`, nil
	}
	hadPrefix := strings.HasPrefix(p, prefix)
	if !hadPrefix && !WindowsIsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	var initial syscall.Errno
	final, err := finalPathName(p)
	switch {
	case err == nil:
		p = final
	case strict:
		return "", &fs.PathError{Op: "resolve", Path: p, Err: err}
	default:
		errors.As(err, &initial)
		if p, err = finalPathLax(p); err != nil {
			return "", err
		}
	}
	if hadPrefix || !strings.HasPrefix(p, prefix) {
		return p, nil
	}
	var short string
	switch {
	case strings.HasPrefix(p, uncPrefix):
		short = `\\` + p[len(uncPrefix):]
	case len(p) >= len(prefix)+3 && p[len(prefix)+1:len(prefix)+3] == `:\`:
		short = p[len(prefix):]
	default:
		return p, nil
	}
	again, err := finalPathName(short)
	var errno syscall.Errno
	if err == nil && again == p || err != nil && errors.As(err, &errno) && errno == initial {
		return short, nil
	}
	return p, nil
}

// Final path of the longest part of p that can be opened, followed by the rest of p as written
func finalPathLax(p string) (string, error) {
	tail := ""
	for p != "" {
		final, err := finalPathName(p)
		if err == nil {
			return joinTail(final, tail), nil
		}
		if !errnoIn(err, finalPathTolerated) {
			return "", &fs.PathError{Op: "resolve", Path: p, Err: err}
		}
		if target, rerr := readlinkDeep(p); rerr == nil && target != p {
			return joinTail(target, tail), nil
		}
		var name string
		if errnoIn(err, finalPathListable) {
			if stored, ferr := storedName(p); ferr == nil {
				p, _ = windowsSplit(p)
				name = stored
			} else {
				p, name = windowsSplit(p)
			}
		} else {
			p, name = windowsSplit(p)
		}
		if p != "" && name == "" {
			return p + tail, nil
		}
		tail = joinTail(name, tail)
	}
	return tail, nil
}

func joinTail(head, tail string) string {
	if tail == "" || strings.HasSuffix(head, `\`) {
		return head + tail
	}
	return head + `\` + tail
}

// Target of p after following its chain of links until one repeats or cannot be read
func readlinkDeep(p string) (string, error) {
	seen := map[string]bool{}
	for key := WindowsFoldCase(p); !seen[key]; key = WindowsFoldCase(p) {
		seen[key] = true
		old := p
		target, err := os.Readlink(p)
		if err != nil {
			if errnoIn(err, readlinkTolerated) {
				break
			}
			return "", err
		}
		p = target
		if !WindowsIsAbs(p) {
			if fi, err := os.Lstat(old); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
				return old, nil
			}
			head, _ := windowsSplit(old)
			p = WindowsJoin(head, p)
		}
	}
	return p, nil
}

// Path GetFinalPathNameByHandleW reports for the opened p
func finalPathName(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}

// Name the directory stores for the entry p matches, in its stored case
func storedName(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(name, &data)
	if err != nil {
		return "", err
	}
	_ = windows.FindClose(h)
	return windows.UTF16ToString(data.FileName[:]), nil
}
