// Package tarzst packs file trees into deterministic zstd compressed tars and serves them as read only file systems
package tarzst

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// One regular file of a tree with its slash path, bytes, digest and permission bits
type File struct {
	Path string
	Data []byte
	Sum  string
	Mode fs.FileMode
}

// File record over the given bytes, its digest computed
func NewFile(rel string, data []byte, mode fs.FileMode) File {
	sum := sha256.Sum256(data)
	return File{Path: rel, Data: data, Sum: hex.EncodeToString(sum[:]), Mode: mode}
}

// Permission bits a packed file carries, executable when any execute bit is set or, when asked, it starts with a shebang
func packedMode(mode fs.FileMode, data []byte, shebangExec bool) fs.FileMode {
	if mode&0o111 != 0 || (shebangExec && bytes.HasPrefix(data, []byte("#!"))) {
		return 0o755
	}
	return 0o644
}

// Every regular file under root in path order, refusing symlinks and other irregular entries
func Collect(root string, shebangExec bool) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, NewFile(filepath.ToSlash(rel), data, packedMode(info.Mode(), data, shebangExec)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortFiles(out)
	return out, nil
}

func sortFiles(files []File) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// Tar of the files in path order with zero times and owners, compressed by a single threaded zstd encoder
func Encode(files []File) ([]byte, error) {
	sorted := append([]File(nil), files...)
	sortFiles(sorted)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, f := range sorted {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: int64(f.Mode.Perm()), Size: int64(len(f.Data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw.Bytes(), nil), nil
}

// Regular files of an archive in path order, refusing entries that are not regular files or escape the tree
func Decode(archive []byte) ([]File, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(archive, nil)
	if err != nil {
		return nil, err
	}
	var out []File
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || !fs.ValidPath(hdr.Name) || hdr.Name == "." {
			return nil, fmt.Errorf("archive entry %q is not a regular file inside the tree", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, NewFile(hdr.Name, data, fs.FileMode(hdr.Mode).Perm()))
	}
	sortFiles(out)
	return out, nil
}

// Manifest text, one path, sha256 and octal mode line per file in path order
func Manifest(files []File) string {
	sorted := append([]File(nil), files...)
	sortFiles(sorted)
	var b strings.Builder
	for _, f := range sorted {
		fmt.Fprintf(&b, "%s %s %04o\n", f.Path, f.Sum, f.Mode.Perm())
	}
	return b.String()
}

// Parses manifest text back into path ordered records without file bytes
func ParseManifest(text string) ([]File, error) {
	var out []File
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("manifest line %q", line)
		}
		mode, err := strconv.ParseUint(fields[2], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("manifest line %q: %w", line, err)
		}
		out = append(out, File{Path: fields[0], Sum: fields[1], Mode: fs.FileMode(mode)})
	}
	return out, nil
}

// Writes every archive file under dir with its permission bits, creating parent directories
func Extract(archive []byte, dir string) error {
	files, err := Decode(archive)
	if err != nil {
		return err
	}
	for _, f := range files {
		target := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := os.WriteFile(target, f.Data, f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(target, f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// Joins a root prefix and a slash path inside it
func under(root, rel string) string {
	if root == "" {
		return rel
	}
	return path.Join(root, rel)
}
