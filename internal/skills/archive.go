package skills

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Read only file system over a tar of a skill tree compressed with zstd, as plugins embed their skills: the tree keeps its executable
// bits, and a skill may hold what a Go package can't, such as a template with its own go.mod. Decoded on first use
func Archive(data []byte) fs.FS {
	return &archive{load: sync.OnceValues(func() (*tree, error) { return decodeArchive(data) })}
}

type archive struct {
	load func() (*tree, error)
}

type tree struct {
	files map[string]*archived
	dirs  map[string][]fs.DirEntry
}

type archived struct {
	name string
	data []byte
	mode fs.FileMode
}

func decodeArchive(data []byte) (*tree, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	t := &tree{files: map[string]*archived{}, dirs: map[string][]fs.DirEntry{}}
	children := map[string]map[string]fs.DirEntry{".": {}}
	reader := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || !fs.ValidPath(header.Name) || header.Name == "." {
			return nil, fmt.Errorf("The archive holds %q, which is not a file inside its folder.", header.Name)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		file := &archived{name: header.Name, data: body, mode: fs.FileMode(header.Mode).Perm()}
		t.files[header.Name] = file
		var child fs.DirEntry = entry{name: path.Base(header.Name), file: file}
		for dir := path.Dir(header.Name); ; dir = path.Dir(dir) {
			if children[dir] == nil {
				children[dir] = map[string]fs.DirEntry{}
			}
			children[dir][child.Name()] = child
			if dir == "." {
				break
			}
			child = entry{name: path.Base(dir)}
		}
	}
	for dir, entries := range children {
		list := make([]fs.DirEntry, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		slices.SortFunc(list, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		t.dirs[dir] = list
	}
	return t, nil
}

func (a *archive) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	t, err := a.load()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if file, ok := t.files[name]; ok {
		return &openFile{archived: file, reader: bytes.NewReader(file.data)}, nil
	}
	if entries, ok := t.dirs[name]; ok {
		return &openDir{name: name, entries: entries}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// One entry of a folder: a file of the archive, or a folder when file is nil
type entry struct {
	name string
	file *archived
}

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return e.file == nil }
func (e entry) Type() fs.FileMode          { return e.info().Mode().Type() }
func (e entry) Info() (fs.FileInfo, error) { return e.info(), nil }

func (e entry) info() info {
	if e.file == nil {
		return info{name: e.name, mode: fs.ModeDir | 0o755}
	}
	return info{name: e.name, size: int64(len(e.file.data)), mode: e.file.mode}
}

type info struct {
	name string
	size int64
	mode fs.FileMode
}

func (i info) Name() string       { return i.name }
func (i info) Size() int64        { return i.size }
func (i info) Mode() fs.FileMode  { return i.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.mode.IsDir() }
func (i info) Sys() any           { return nil }

type openFile struct {
	*archived
	reader *bytes.Reader
}

func (f *openFile) Stat() (fs.FileInfo, error) {
	return info{name: path.Base(f.name), size: int64(len(f.data)), mode: f.mode}, nil
}
func (f *openFile) Read(p []byte) (int, error) { return f.reader.Read(p) }
func (f *openFile) Close() error               { return nil }

type openDir struct {
	name    string
	entries []fs.DirEntry
	read    int
}

func (d *openDir) Stat() (fs.FileInfo, error) {
	return info{name: path.Base(d.name), mode: fs.ModeDir | 0o755}, nil
}
func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}
func (d *openDir) Close() error { return nil }

func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.read:]
	if n <= 0 {
		d.read = len(d.entries)
		return slices.Clone(rest), nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	rest = rest[:min(n, len(rest))]
	d.read += len(rest)
	return slices.Clone(rest), nil
}
