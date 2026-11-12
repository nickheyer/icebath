package files

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const readDirStatConcurrency = 16

// Noise the project tree always hides, git or not
var readDirHidden = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".cache": true, ".next": true, ".turbo": true, ".venv": true,
	"__pycache__": true, "build": true, "dist": true, "node_modules": true, "target": true, "venv": true,
}

// Dirent is one directory entry as readdir withFileTypes reports it
type Dirent interface {
	Name() string
	IsDirectory() bool
	IsFile() bool
	IsSymbolicLink() bool
}

// EntryStat is the stat answer the listing needs for a link or unknown entry
type EntryStat interface {
	IsDir() bool
}

// DirFS is the file system a directory listing reads
type DirFS interface {
	StatFS
	ReadDir(name string) ([]Dirent, error)
	StatEntry(name string) (EntryStat, error)
}

type osDirent struct{ e os.DirEntry }

func (d osDirent) Name() string         { return d.e.Name() }
func (d osDirent) IsDirectory() bool    { return d.e.IsDir() }
func (d osDirent) IsFile() bool         { return d.e.Type().IsRegular() }
func (d osDirent) IsSymbolicLink() bool { return d.e.Type()&os.ModeSymlink != 0 }

func osReadDir(name string) ([]Dirent, error) {
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, nodeFsError(err, "scandir", name)
	}
	out := make([]Dirent, len(entries))
	for i, e := range entries {
		out[i] = osDirent{e}
	}
	return out, nil
}

// DirEntry is one row of the project tree listing
type DirEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	IsDirectory bool   `json:"isDirectory"`
}

// ReadDirResult is the directory listing answer, an error code instead of a throw
type ReadDirResult struct {
	Entries []DirEntry `json:"entries"`
	Error   string     `json:"error,omitempty"`
}

var localeCollator = struct {
	sync.Mutex
	c *collate.Collator
}{c: collate.New(language.Und)}

// Ordering of String.prototype.localeCompare under the root locale
func localeCompare(a, b string) int {
	localeCollator.Lock()
	defer localeCollator.Unlock()
	return localeCollator.c.CompareString(a, b)
}

func entryForDirent(d Dirent, resolved string, fsys DirFS) DirEntry {
	full := filepath.Join(resolved, d.Name())
	isDir := d.IsDirectory()
	if !isDir && (d.IsSymbolicLink() || !d.IsFile()) {
		if st, err := fsys.StatEntry(full); err == nil {
			isDir = st.IsDir()
		} else {
			isDir = false
		}
	}
	return DirEntry{Name: d.Name(), Path: full, IsDirectory: isDir}
}

// Project tree listing of a directory, hidden noise dropped, folders first, at most 16 stats at once
func ReadDirForIpc(dirPath any, fsys DirFS) ReadDirResult {
	if fsys == nil {
		fsys = osFS{}
	}
	raw := ""
	if s, ok := dirPath.(string); ok {
		raw = s
	} else if dirPath != nil {
		raw = jsString(dirPath)
	}
	readPath := ResolveLocalReadPath(raw, "", nil)
	dir, err := ResolveDirectoryForIpc(readPath, "Directory read", "", fsys)
	if err != nil {
		return readDirFailure(err)
	}
	dirents, err := fsys.ReadDir(dir.ResolvedPath)
	if err != nil {
		return readDirFailure(err)
	}
	visible := make([]Dirent, 0, len(dirents))
	for _, d := range dirents {
		if !readDirHidden[d.Name()] {
			visible = append(visible, d)
		}
	}
	entries := make([]DirEntry, len(visible))
	workers := readDirStatConcurrency
	if len(visible) < workers {
		workers = len(visible)
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				entries[i] = entryForDirent(visible[i], dir.ResolvedPath, fsys)
			}
		}()
	}
	for i := range visible {
		next <- i
	}
	close(next)
	wg.Wait()
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDirectory != entries[j].IsDirectory {
			return entries[i].IsDirectory
		}
		return localeCompare(entries[i].Name, entries[j].Name) < 0
	})
	return ReadDirResult{Entries: entries}
}

func readDirFailure(err error) ReadDirResult {
	code := ErrorCode(err)
	if code == "" {
		code = "read-error"
	}
	return ReadDirResult{Entries: []DirEntry{}, Error: code}
}
