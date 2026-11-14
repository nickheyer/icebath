package toolchain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Desktop metadata names that never decide a layout or a digest
var junkNames = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true, ".localized": true}

// Top level directory names that are a layout of their own and stay in place when flattening
var layoutDirs = map[string]bool{"bin": true, "cmd": true, "lib": true, "libexec": true, "share": true, "etc": true, "usr": true}

// Delays a rename waits out while a scanner still holds a freshly written tree
var heldDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// Renames src to dst, riding out a short hold on either
func renameHeld(ctx context.Context, src, dst string) error {
	return fileutil.RenameRetrying(ctx, src, dst, nil, heldDelays...)
}

// Reports an archive name the store can unpack
func archiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".zip", ".whl", ".deb"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// Empties dest and unpacks the archive into it by its file name, links kept only inside dest
func extract(ctx context.Context, name, archive, dest string) error {
	if err := removeAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	lower := strings.ToLower(filepath.Base(archive))
	switch {
	case strings.HasSuffix(lower, ".zip"), strings.HasSuffix(lower, ".whl"):
		return fileutil.ExtractZip(ctx, archive, dest, fileutil.ContainedLinks)
	case strings.HasSuffix(lower, ".deb"):
		return unpackDeb(ctx, name, archive, dest)
	case archiveName(lower):
		return fileutil.ExtractTarFile(ctx, archive, dest, fileutil.ContainedLinks, fileutil.AnyCompression)
	}
	return fmt.Errorf("unsupported archive: %s", filepath.Base(archive))
}

// Removes path whatever it is, a missing one counting as removed
func removeAll(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 && !fileutil.IsJunction(path) {
		return fileutil.RemoveTree(path, true)
	}
	return os.Remove(path)
}

// Unpacks the data tarball of a Debian package into dest and makes every file readable by all users
func unpackDeb(ctx context.Context, name, archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	base := filepath.Base(archive)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, []byte("!<arch>\n")) {
		return &InstallError{Name: name, Cause: "not an ar archive: " + base}
	}
	header := make([]byte, 60)
	for offset := int64(8); ; {
		if _, err := f.ReadAt(header, offset); err != nil {
			return &InstallError{Name: name, Cause: "no data.tar member in " + base}
		}
		member := strings.TrimRight(string(header[:16]), " ")
		size, err := strconv.ParseInt(strings.TrimSpace(string(header[48:58])), 10, 64)
		if err != nil || size < 0 {
			return &InstallError{Name: name, Cause: "bad ar member size in " + base}
		}
		start := offset + 60
		if !strings.HasPrefix(member, "data.tar") {
			offset = start + size + size%2
			continue
		}
		if strings.HasSuffix(strings.TrimRight(member, "/"), ".lzma") {
			return &InstallError{Name: name, Cause: fmt.Sprintf("unsupported data compression %s in %s", member, base)}
		}
		if err := fileutil.ExtractTar(ctx, io.NewSectionReader(f, start, size), dest, fileutil.ContainedLinks, fileutil.AnyCompression); err != nil {
			var unsafe *fileutil.UnsafeMemberError
			if errors.As(err, &unsafe) {
				return &InstallError{Name: name, Cause: fmt.Sprintf("unsafe member %q: %v", unsafe.Member, err), Err: err}
			}
			return err
		}
		return normalizeModes(dest)
	}
}

// Gives every regular file of root mode 0644, plus execute for all when any execute bit was set
func normalizeModes(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		wanted := fs.FileMode(0o644)
		if fi.Mode()&0o111 != 0 {
			wanted |= 0o111
		}
		if err := os.Chmod(p, wanted); err != nil && !errors.Is(err, fs.ErrPermission) {
			return err
		}
		return nil
	})
}

// Reports an inert regular metadata file a desktop wrote, never a link, directory or executable
func osMetadata(path string) bool {
	name := filepath.Base(path)
	if !junkNames[name] && !strings.HasPrefix(name, "._") {
		return false
	}
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 == 0
}

// Hoists the contents of a lone top level directory unless that directory is itself the layout
func flattenSingleDir(ctx context.Context, dest string) error {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	var real []fs.DirEntry
	for _, e := range entries {
		if !osMetadata(filepath.Join(dest, e.Name())) {
			real = append(real, e)
		}
	}
	if len(real) != 1 || !real[0].IsDir() || layoutDirs[real[0].Name()] {
		return nil
	}
	inner := filepath.Join(dest, real[0].Name())
	items, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, item := range items {
		target := filepath.Join(dest, item.Name())
		if _, err := os.Lstat(target); err == nil && !(osMetadata(filepath.Join(inner, item.Name())) && osMetadata(target)) {
			return nil
		}
	}
	for _, item := range items {
		target := filepath.Join(dest, item.Name())
		if _, err := os.Lstat(target); err == nil {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if err := renameHeld(ctx, filepath.Join(inner, item.Name()), target); err != nil {
			return err
		}
	}
	return os.Remove(inner)
}

// Moves every file of src into dst at the same relative path, refusing a file both trees hold
func mergeTree(ctx context.Context, src, dst string) error {
	var files []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, item := range files {
		rel, err := filepath.Rel(src, item)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("archives disagree about %s", filepath.ToSlash(rel))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := renameHeld(ctx, item, target); err != nil {
			return err
		}
	}
	return nil
}

// Digest over the sorted relative paths and contents of a tree, links hashed by target text, metadata skipped when asked
func treeDigest(root string, skipMetadata bool) (string, error) {
	type item struct {
		rel  string
		path string
		link bool
	}
	var items []item
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		link := d.Type()&fs.ModeSymlink != 0 || fileutil.IsJunction(p)
		switch {
		case link:
			items = append(items, item{filepath.ToSlash(rel), p, true})
			if d.IsDir() {
				return filepath.SkipDir
			}
		case d.IsDir():
		case skipMetadata && osMetadata(p):
		default:
			items = append(items, item{filepath.ToSlash(rel), p, false})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	h := sha256.New()
	for _, it := range items {
		h.Write([]byte(it.rel))
		h.Write([]byte{0})
		if it.link {
			target, err := os.Readlink(it.path)
			if err != nil {
				return "", err
			}
			h.Write([]byte(target))
			continue
		}
		f, err := os.Open(it.path)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		if err := errors.Join(err, f.Close()); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Reports whether the tree at root hashes to recorded, with or without desktop metadata counted
func digestMatches(root, recorded string) bool {
	if recorded == "" {
		return false
	}
	for _, skip := range []bool{true, false} {
		if got, err := treeDigest(root, skip); err == nil && got == recorded {
			return true
		}
	}
	return false
}

// Bytes of every regular file below root, links not followed
func treeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// Verdict of an executable header against a target architecture
type archVerdict int

// A header that matches, one that names another architecture, and a file with no such header
const (
	archMatches archVerdict = iota
	archDiffers
	archUnknown
)

// Compares the PE, ELF or Mach-O machine of binary with the architecture of target
func machineMatches(binary string, target Target) archVerdict {
	f, err := os.Open(binary)
	if err != nil {
		return archUnknown
	}
	defer f.Close()
	head := make([]byte, 64)
	if n, _ := io.ReadFull(f, head); n < 64 {
		return archUnknown
	}
	arch := target.arch()
	verdict := func(match bool) archVerdict {
		if match {
			return archMatches
		}
		return archDiffers
	}
	le := binary_le
	switch {
	case head[0] == 'M' && head[1] == 'Z':
		sig := make([]byte, 6)
		if _, err := f.ReadAt(sig, int64(le.Uint32(head[60:64]))); err != nil || !bytes.Equal(sig[:4], []byte("PE\x00\x00")) {
			return archUnknown
		}
		want := map[string]uint16{"x64": 0x8664, "arm64": 0xAA64}[arch]
		return verdict(le.Uint16(sig[4:6]) == want)
	case bytes.Equal(head[:4], []byte("\x7fELF")):
		want := map[string]uint16{"x64": 0x3E, "arm64": 0xB7}[arch]
		return verdict(le.Uint16(head[18:20]) == want)
	case bytes.Equal(head[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}), bytes.Equal(head[:4], []byte{0xfe, 0xed, 0xfa, 0xcf}):
		want := map[string]uint32{"x64": 0x01000007, "arm64": 0x0100000C}[arch]
		return verdict(le.Uint32(head[4:8]) == want)
	case bytes.Equal(head[:4], []byte{0xca, 0xfe, 0xba, 0xbe}), bytes.Equal(head[:4], []byte{0xbe, 0xba, 0xfe, 0xca}):
		return archMatches
	}
	return archUnknown
}

// Little endian decoder the executable headers are read with
var binary_le = binary.LittleEndian

// Top level names of an install directory for a verification diagnosis
func entryListing(entry string) string {
	entries, err := os.ReadDir(entry)
	if err != nil {
		return "store entry does not exist"
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	const limit = 12
	shown := strings.Join(names[:min(len(names), limit)], ", ")
	if len(names) > limit {
		shown += fmt.Sprintf(", … (%d entries)", len(names))
	}
	return shown
}
