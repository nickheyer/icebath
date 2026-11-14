//go:build windows

package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/internal/home"
)

const (
	longPrefix    = `\\?\`
	longUNCPrefix = `\\?\UNC\`
)

func canonical(p string) (string, error) {
	abs, err := filepath.Abs(home.ExpandUser(p))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(abs, longPrefix) {
		if full, err := finalPath(abs); err == nil {
			return full, nil
		}
		return nonstrictFinal(abs), nil
	}
	full, ferr := finalPath(abs)
	if ferr != nil {
		full = nonstrictFinal(abs)
	}
	return stripLongPrefix(full, ferr), nil
}

func finalPath(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

func nonstrictFinal(abs string) string {
	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		if full, err := finalPath(cur); err == nil {
			return strings.TrimRight(full, `\`) + `\` + strings.Join(tail, `\`)
		}
	}
}

func stripLongPrefix(p string, initial error) string {
	if !strings.HasPrefix(p, longPrefix) {
		return p
	}
	short := p[len(longPrefix):]
	if strings.HasPrefix(p, longUNCPrefix) {
		short = `\\` + p[len(longUNCPrefix):]
	}
	got, err := finalPath(short)
	if err == nil && got == p {
		return short
	}
	var a, b windows.Errno
	if err != nil && initial != nil && errors.As(err, &a) && errors.As(initial, &b) && a == b {
		return short
	}
	return p
}

func joinRelative(root, rel string) (string, bool) {
	vol := filepath.VolumeName(rel)
	switch {
	case vol != "":
		if !strings.EqualFold(vol, filepath.VolumeName(root)) {
			return "", false
		}
		rest := rel[len(vol):]
		if strings.HasPrefix(rest, `\`) || strings.HasPrefix(rest, "/") {
			return vol + rest, true
		}
		return strings.TrimRight(root, `\/`) + `\` + rest, true
	case strings.HasPrefix(rel, `\`) || strings.HasPrefix(rel, "/"):
		return filepath.VolumeName(root) + rel, true
	}
	return strings.TrimRight(root, `\/`) + `\` + rel, true
}

func relativeTo(child, root string) (string, bool) {
	c, r := filepath.Clean(child), filepath.Clean(root)
	if strings.EqualFold(c, r) {
		return ".", true
	}
	prefix := r
	if !strings.HasSuffix(prefix, `\`) {
		prefix += `\`
	}
	if len(c) > len(prefix) && strings.EqualFold(c[:len(prefix)], prefix) {
		return c[len(prefix):], true
	}
	return "", false
}

func isSeparator(r rune) bool { return r == '/' || r == '\\' }

func isMount(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	vol := filepath.VolumeName(abs)
	rest := strings.TrimLeft(abs[len(vol):], `\/`)
	if vol != "" && (vol[0] == '\\' || vol[0] == '/') {
		return rest == ""
	}
	if len(abs) > len(vol) && rest == "" {
		return true
	}
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(name, &buf[0], uint32(len(buf))); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimRight(abs, `\/`), strings.TrimRight(windows.UTF16ToString(buf), `\/`))
}

func makeRemovable(path string) {
	_ = os.Chmod(path, 0o666)
}
