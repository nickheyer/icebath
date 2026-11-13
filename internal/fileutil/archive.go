package fileutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Refusal texts an extraction reports for an archive member
const (
	reasonUnsafePath    = "Unsafe archive member path"
	reasonUnsupported   = "Unsupported archive member type"
	reasonAbsolute      = "Archive member has an absolute path"
	reasonOutside       = "Archive member would be extracted outside the destination"
	reasonSpecial       = "Archive member is a special file"
	reasonAbsoluteLink  = "Archive member is a link to an absolute path"
	reasonLinkOutside   = "Archive member would link outside the destination"
	reasonLinkMissing   = "Archive member links to a member the archive does not hold"
	reasonSymlinkMember = "ZIP contains unsupported symlink member"
	reasonZipSlip       = "Zip-slip detected, archive member escapes extraction directory"
	reasonEmptyName     = "Archive member has an empty file name"
	reasonOverlap       = "Archive member overlaps another member, a possible zip bomb"
	reasonBadLink       = "Archive member is not a usable symlink target"
)

// Archive member an extraction refused, with the reason and the location it would have reached
type UnsafeMemberError struct {
	Member string
	Reason string
	Target string
}

// Reason and member, followed by the location reached when there is one
func (e *UnsafeMemberError) Error() string {
	if e.Target != "" {
		return fmt.Sprintf("%s: %s -> %s", e.Reason, e.Member, e.Target)
	}
	return e.Reason + ": " + e.Member
}

// Members an extraction accepts and how it treats their names
type ExtractPolicy int

const (
	// Directories and regular files only, a link, special member or escaping name refusing the archive
	FilesOnly ExtractPolicy = iota
	// Directories, regular files and links that stay inside the destination, special members refused
	ContainedLinks
)

// Path parts of an archive member name, refusing absolute, drive, empty and parent references
func MemberParts(name string) ([]string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	var parts []string
	for _, p := range strings.Split(normalized, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	drive := len(name) >= 2 && name[1] == ':'
	if normalized == "" || strings.HasPrefix(normalized, "/") || drive || len(parts) == 0 || slices.Contains(parts, "..") || slices.ContainsFunc(parts, platformDotName) {
		return nil, &UnsafeMemberError{Member: name, Reason: reasonUnsafePath}
	}
	return parts, nil
}

// Everything before the last separator of p without the lexical cleaning filepath.Dir applies
func rawDir(p string) string {
	if i := strings.LastIndexByte(p, filepath.Separator); i >= 0 {
		return p[:i]
	}
	return "."
}

// Where a member at rel under root lands, its parent resolved and its final element left for the operation
func placement(root, rel string) (string, error) {
	base := rel[strings.LastIndexByte(rel, filepath.Separator)+1:]
	if base == "" || base == "." || base == ".." {
		return resolveLax(root + string(filepath.Separator) + rel)
	}
	dir, err := resolveLax(root + string(filepath.Separator) + rawDir(rel))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, base), nil
}

// Path a member name reaches under dest the way the platform joins an archive name, an absolute name replacing dest
func joinMember(dest, name string) string {
	native := filepath.FromSlash(name)
	if filepath.IsAbs(native) {
		return native
	}
	sep := string(filepath.Separator)
	if vol := filepath.VolumeName(native); vol != "" {
		if !samePathText(vol, filepath.VolumeName(dest)) {
			return native
		}
		native = native[len(vol):]
	}
	if strings.HasPrefix(native, sep) {
		return filepath.VolumeName(dest) + native
	}
	return dest + sep + native
}

// Writes the bytes of r to place, creating or truncating it
func writeMember(place string, r io.Reader) error {
	out, err := os.OpenFile(place, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		return closeWith(err, out)
	}
	return out.Close()
}

// Gives an extracted member its mode and times, a filesystem keeping neither leaving the member as written
func stampMember(place string, mode fs.FileMode, mtime time.Time) {
	_ = os.Chmod(place, mode)
	_ = os.Chtimes(place, mtime, mtime)
}

// Clears whatever non-directory sits at place so a link can be created there
func clearPlace(place, op string) error {
	fi, err := os.Lstat(place)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 && !isMountPoint(place) {
		return &fs.PathError{Op: op, Path: place, Err: syscall.EISDIR}
	}
	return os.Remove(place)
}

// Writes text as the regular file standing in for a symlink the system refused, in platform line endings
func writeLinkText(place, text string) error {
	return os.WriteFile(place, []byte(nativeLineEndings(text)), 0o666)
}
