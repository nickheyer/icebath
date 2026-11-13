//go:build windows

package fileutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Target a symlink or junction at path names, false for anything else
func linkTarget(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	if fi.Mode()&fs.ModeSymlink == 0 && !(reparsePoint(fi) && isMountPoint(path)) {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	return target, true
}

// Text as Windows stores it, every LF written as CRLF
func nativeLineEndings(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// Zip member name with the Windows separator turned into the forward slash the format requires
func zipSlashes(name string) string { return strings.ReplaceAll(name, `\`, "/") }

// Host system a written zip records in its version made by field, 0 for MS-DOS and Windows
const zipCreatorSystem = 0

// Archive name parts with characters Windows forbids replaced by underscores and trailing dots and spaces dropped
func legalNameParts(parts []string) []string {
	var out []string
	for _, p := range parts {
		p = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`:<>|"?*`, r) {
				return '_'
			}
			return r
		}, p)
		if p = strings.TrimRight(p, " ."); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Reports whether two path strings name the same location on a case insensitive filesystem
func samePathText(a, b string) bool { return strings.EqualFold(a, b) }

// Reports whether the attributes behind fi mark a reparse point
func reparsePoint(fi fs.FileInfo) bool {
	data, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// Reports whether path itself is an NTFS junction, never following it
func isMountPoint(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(p, &data)
	if err != nil {
		return false
	}
	_ = windows.FindClose(h)
	return data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && data.Reserved0 == windows.IO_REPARSE_TAG_MOUNT_POINT
}

// Reports whether part holds only dots and spaces beyond the plain dot entries, a name Windows trims
func platformDotName(part string) bool {
	return part != "" && part != "." && part != ".." && strings.Trim(part, ". ") == ""
}

// Execute bits Windows stat results carry for .exe, .bat, .cmd and .com files
func synthesizedExec(path string) fs.FileMode {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".exe", ".bat", ".cmd", ".com":
		return 0o111
	}
	return 0
}
