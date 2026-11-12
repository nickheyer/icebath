package tarzst

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"sync"
	"time"
)

// FS serves an embedded archive like embed.FS, read only files and synthesized directories under an optional root
type FS struct {
	load func() (*tree, error)
}

// Decoded archive indexed by full slash path, files and the sorted entries of every directory
type tree struct {
	files map[string]*File
	dirs  map[string][]fs.DirEntry
}

// File system over archive bytes decoded on first use, every path placed under root when root is set
func NewFS(archive []byte, root string) *FS {
	return &FS{load: sync.OnceValues(func() (*tree, error) {
		files, err := Decode(archive)
		if err != nil {
			return nil, err
		}
		t := &tree{files: map[string]*File{}, dirs: map[string][]fs.DirEntry{}}
		children := map[string]map[string]fs.DirEntry{".": {}}
		for i := range files {
			full := under(root, files[i].Path)
			t.files[full] = &files[i]
			child := fs.FileInfoToDirEntry(fileInfo{name: path.Base(full), size: int64(len(files[i].Data))})
			for dir := path.Dir(full); ; dir = path.Dir(dir) {
				if children[dir] == nil {
					children[dir] = map[string]fs.DirEntry{}
				}
				children[dir][child.Name()] = child
				if dir == "." {
					break
				}
				child = fs.FileInfoToDirEntry(fileInfo{name: path.Base(dir), dir: true})
			}
		}
		for dir, entries := range children {
			list := make([]fs.DirEntry, 0, len(entries))
			for _, e := range entries {
				list = append(list, e)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
			t.dirs[dir] = list
		}
		return t, nil
	})}
}

// Node at name, a file record or a directory listing, with the error embed.FS reports for a bad or absent path
func (f *FS) lookup(op, name string) (*File, []fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	t, err := f.load()
	if err != nil {
		return nil, nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	if file, ok := t.files[name]; ok {
		return file, nil, nil
	}
	if entries, ok := t.dirs[name]; ok {
		return nil, entries, nil
	}
	return nil, nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
}

// Opens a file for reading or a directory for listing
func (f *FS) Open(name string) (fs.File, error) {
	file, entries, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	if file != nil {
		return &openFile{name: name, file: file, reader: bytes.NewReader(file.Data)}, nil
	}
	return &openDir{name: name, entries: entries}, nil
}

// Copy of a file's bytes
func (f *FS) ReadFile(name string) ([]byte, error) {
	file, _, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("is a directory")}
	}
	return bytes.Clone(file.Data), nil
}

// Sorted entries of a directory
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, entries, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	if file != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("not a directory")}
	}
	return append([]fs.DirEntry(nil), entries...), nil
}

// File info of a path, modes as embed.FS reports them
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	file, _, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	if file != nil {
		return fileInfo{name: path.Base(name), size: int64(len(file.Data))}, nil
	}
	return fileInfo{name: path.Base(name), dir: true}, nil
}

// Info of one node, read only file mode 0444 and directory mode 0555 like embed.FS
type fileInfo struct {
	name string
	size int64
	dir  bool
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }

func (i fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

// Open file reading from its archived bytes
type openFile struct {
	name   string
	file   *File
	reader *bytes.Reader
}

func (o *openFile) Stat() (fs.FileInfo, error) {
	return fileInfo{name: path.Base(o.name), size: int64(len(o.file.Data))}, nil
}
func (o *openFile) Read(p []byte) (int, error) { return o.reader.Read(p) }
func (o *openFile) Seek(offset int64, whence int) (int64, error) {
	return o.reader.Seek(offset, whence)
}
func (o *openFile) ReadAt(p []byte, off int64) (int, error) { return o.reader.ReadAt(p, off) }
func (o *openFile) Close() error                            { return nil }

// Open directory listing its entries in name order
type openDir struct {
	name    string
	entries []fs.DirEntry
	offset  int
}

func (d *openDir) Stat() (fs.FileInfo, error) {
	return fileInfo{name: path.Base(d.name), dir: true}, nil
}
func (d *openDir) Close() error { return nil }
func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

// Next n entries, or every remaining entry when n is not positive
func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n <= 0 {
		d.offset = len(d.entries)
		return append([]fs.DirEntry(nil), rest...), nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(rest))
	d.offset += n
	return append([]fs.DirEntry(nil), rest[:n]...), nil
}
