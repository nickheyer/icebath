package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unicode"
)

// Python os.path.normpath, posixpath rules keeping a double leading slash, ntpath rules on Windows
func pyNormpath(p string) string {
	if runtime.GOOS == "windows" {
		return ntNormpath(p)
	}
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, comp := range strings.Split(p, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if comp != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// ntpath.normpath, slashes turned to backslashes and parent links collapsed below the drive and root
func ntNormpath(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	drive := filepath.VolumeName(p)
	rest, root := p[len(drive):], ""
	if strings.HasPrefix(rest, `\`) {
		rest, root = rest[1:], `\`
	}
	comps := strings.Split(rest, `\`)
	for i := 0; i < len(comps); {
		switch {
		case comps[i] == "" || comps[i] == ".":
			comps = slices.Delete(comps, i, i+1)
		case comps[i] == ".." && i > 0 && comps[i-1] != "..":
			comps = slices.Delete(comps, i-1, i+1)
			i--
		case comps[i] == ".." && i == 0 && root != "":
			comps = slices.Delete(comps, i, i+1)
		default:
			i++
		}
	}
	if drive+root == "" && len(comps) == 0 {
		comps = []string{"."}
	}
	return drive + root + strings.Join(comps, `\`)
}

// Python os.path.abspath, the getcwd failure of a relative path returned as the OSError Python raises
func pyAbspath(p string) (string, error) {
	if runtime.GOOS == "windows" {
		return filepath.Abs(p)
	}
	if !strings.HasPrefix(p, "/") {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = strings.TrimSuffix(cwd, "/") + "/" + p
	}
	return pyNormpath(p), nil
}

// Python os.path.realpath without strict, resolving symlinks and keeping missing tails literal
func pyRealpath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		if _, err := os.Getwd(); err != nil {
			return "", err
		}
	}
	return realpathLax(p), nil
}

// Index just past the last path separator of p, both slashes counting on Windows
func pyLastSep(p string) int {
	if runtime.GOOS == "windows" {
		return strings.LastIndexAny(p, `/\`) + 1
	}
	return strings.LastIndex(p, "/") + 1
}

// Python os.path.basename, the text after the last separator and empty for a trailing separator
func pyBasename(p string) string {
	if runtime.GOOS == "windows" {
		p = p[len(filepath.VolumeName(p)):]
	}
	return p[pyLastSep(p):]
}

// Python os.path.dirname, trailing separators stripped unless the head is only separators
func pyDirname(p string) string {
	drive := ""
	if runtime.GOOS == "windows" {
		drive = filepath.VolumeName(p)
		p = p[len(drive):]
	}
	head := p[:pyLastSep(p)]
	seps := "/"
	if runtime.GOOS == "windows" {
		seps = `/\`
	}
	if trimmed := strings.TrimRight(head, seps); trimmed != "" {
		head = trimmed
	}
	return drive + head
}

// Drive, root and components of a pathlib path, empty and dot components dropped
func pathlibParts(p string) (anchor string, parts []string, joiner string) {
	isSep, joiner := func(r rune) bool { return r == '/' }, "/"
	drive, root := "", ""
	if runtime.GOOS == "windows" {
		isSep, joiner = func(r rune) bool { return r == '/' || r == '\\' }, `\`
		drive = filepath.VolumeName(p)
		if rest := p[len(drive):]; rest != "" && isSep(rune(rest[0])) {
			root = `\`
		}
	} else if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		root = "//"
	} else if strings.HasPrefix(p, "/") {
		root = "/"
	}
	for _, part := range strings.FieldsFunc(p[len(drive):], isSep) {
		if part != "." {
			parts = append(parts, part)
		}
	}
	return drive + root, parts, joiner
}

// Final component of a pathlib path, empty for a bare anchor or empty path
func pathlibName(p string) string {
	_, parts, _ := pathlibParts(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// pathlib str of a path, repeated separators and dot components dropped and the trailing separator stripped
func pathlibStr(p string) string {
	anchor, parts, joiner := pathlibParts(p)
	if out := anchor + strings.Join(parts, joiner); out != "" {
		return out
	}
	return "."
}

// Python str of an OSError, the errno tag, strerror and quoted filename around a Go error
func pyOSErrorStr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		if text, ok := pyErrnoText(pe.Err); ok {
			return text + ": " + pyRepr(pe.Path)
		}
		return err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		if text, ok := pyErrnoText(le.Err); ok {
			return text + ": " + pyRepr(le.Old) + " -> " + pyRepr(le.New)
		}
		return err.Error()
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		var errno syscall.Errno
		if errors.As(se.Err, &errno) {
			return fmt.Sprintf("[Errno %d] (originated from %s)", uint(errno), se.Syscall)
		}
		return err.Error()
	}
	if text, ok := pyErrnoText(err); ok {
		return text
	}
	return err.Error()
}

// Bracketed errno and C strerror text of a syscall error, WinError and its trimmed message on Windows
func pyErrnoText(err error) (string, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	msg := errno.Error()
	if runtime.GOOS == "windows" {
		msg = strings.TrimRightFunc(msg, func(r rune) bool { return r <= ' ' || r == '.' })
		return fmt.Sprintf("[WinError %d] %s", uint(errno), msg), true
	}
	if r := []rune(msg); len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
		msg = string(r)
	}
	return fmt.Sprintf("[Errno %d] %s", uint(errno), msg), true
}

// Whether every rune of a non empty name is a decimal digit, like str.isdigit on procfs names
func pyIsDigits(s string) bool {
	for _, r := range s {
		if _, ok := pyDigitValue(r); !ok {
			return false
		}
	}
	return s != ""
}
