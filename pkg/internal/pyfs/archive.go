package pyfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Error for an unsafe or unsupported archive member, the ValueError of hermes_cli.archive_safe
type ArchiveError struct{ Msg string }

func (e *ArchiveError) Error() string { return e.Msg }

// Safe relative parts of an archive member name, refusing absolute, drive, empty and parent references
func NormalizeArchiveParts(member string) ([]string, error) {
	normalized := strings.ReplaceAll(member, "\\", "/")
	var parts []string
	for _, p := range strings.Split(normalized, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	if normalized == "" || strings.HasPrefix(normalized, "/") || windowsDrive(member) || len(parts) == 0 || slices.Contains(parts, "..") {
		return nil, &ArchiveError{Msg: "Unsafe archive member path: " + member}
	}
	return parts, nil
}

// Reports whether ntpath.splitdrive finds a drive letter or UNC prefix in name
func windowsDrive(name string) bool {
	if len(name) >= 2 && name[1] == ':' {
		return true
	}
	return len(name) >= 2 && (name[0] == '/' || name[0] == '\\') && (name[1] == '/' || name[1] == '\\')
}

// Writes base.tar.gz of rootDir/baseDir in GNU format through a sibling temp file renamed on completion
func MakeTargz(base, rootDir, baseDir string) (string, error) {
	archivePath := base + ".tar.gz"
	destDir := filepath.Dir(archivePath)
	f, err := os.CreateTemp(destDir, ".archive_*.tar.gz.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return fail(err)
	}
	tw := tar.NewWriter(gz)
	if err := tarAdd(tw, filepath.Join(rootDir, baseDir), baseDir); err != nil {
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, archivePath); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return archivePath, nil
}

// Adds a path like TarFile.add, recursing into directories in sorted order and storing links unfollowed
func tarAdd(tw *tar.Writer, path, arcname string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket != 0 {
		return nil
	}
	link := ""
	if st.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(st, link)
	if err != nil {
		return err
	}
	hdr.Format = tar.FormatGNU
	hdr.Name = filepath.ToSlash(arcname)
	if st.IsDir() {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if st.Mode().IsRegular() {
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if !st.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tarAdd(tw, filepath.Join(path, n), arcname+"/"+n); err != nil {
			return err
		}
	}
	return nil
}

// Visits every member header of a gzip tar archive in order
func eachMember(archive string, visit func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := visit(hdr, tr); err != nil {
			return err
		}
	}
}

// Reports whether a member is a regular file the way TarInfo.isfile does
func memberIsFile(h *tar.Header) bool {
	return h.Typeflag == tar.TypeReg || h.Typeflag == '\x00' || h.Typeflag == tar.TypeCont || h.Typeflag == tar.TypeGNUSparse
}

// Extracts only directories and regular files, failing on links, devices and escaping names
func SafeExtractTargz(archive, destination string) error {
	return eachMember(archive, func(h *tar.Header, r io.Reader) error {
		parts, err := NormalizeArchiveParts(h.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(append([]string{destination}, parts...)...)
		if h.Typeflag == tar.TypeDir {
			return os.MkdirAll(target, 0o777)
		}
		if !memberIsFile(h) {
			return &ArchiveError{Msg: "Unsupported archive member type: " + h.Name}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, r)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		os.Chmod(target, os.FileMode(h.Mode&0o777))
		return nil
	})
}

// Top level directory names of an archive, counting a root only through a nested member or a directory member
func ArchiveRootDirs(archive string) (map[string]bool, error) {
	roots := map[string]bool{}
	err := eachMember(archive, func(h *tar.Header, _ io.Reader) error {
		parts, err := NormalizeArchiveParts(h.Name)
		if err != nil {
			return err
		}
		if len(parts) > 1 || h.Typeflag == tar.TypeDir {
			roots[parts[0]] = true
		}
		return nil
	})
	return roots, err
}

// Copies the regular files under src into dst skipping symlinks, returning the count
func CopyRegularFiles(src, dst string) (int, error) {
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return 0, nil
	}
	var files []string
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != src && d.Type()&os.ModeSymlink == 0 && d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	copied := 0
	for _, p := range files {
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return copied, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return copied, err
		}
		if err := os.WriteFile(target, data, 0o666); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}
