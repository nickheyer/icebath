//go:build unix

package fileutil

import (
	"io/fs"
	"os"
)

// Target a symlink at path names, false for anything that is not a readable symlink
func linkTarget(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	return target, true
}

// Reports whether two path strings name the same location on a case sensitive filesystem
func samePathText(a, b string) bool { return a == b }

// Reports whether path is an NTFS junction, which only Windows has
func isMountPoint(string) bool { return false }

// Text as POSIX stores it, LF line endings unchanged
func nativeLineEndings(s string) string { return s }

// Host system a written zip records in its version made by field, 3 for UNIX
const zipCreatorSystem = 3

// Archive name parts as POSIX accepts them, every byte but the separator being legal
func legalNameParts(parts []string) []string { return parts }

// Zip member name as read on POSIX, where a backslash is an ordinary character
func zipSlashes(name string) string { return name }

// POSIX takes every name made of dots and spaces literally, so none hides a relative reference
func platformDotName(string) bool { return false }

// POSIX mode bits come from the file itself, so no execute bit is inferred from a name
func synthesizedExec(string) fs.FileMode { return 0 }
