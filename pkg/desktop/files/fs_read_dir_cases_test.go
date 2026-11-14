package files

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func entryNames(r ReadDirResult) []string {
	out := []string{}
	for _, e := range r.Entries {
		out = append(out, e.Name)
	}
	return out
}

type fakeDirent struct {
	name               string
	dir, file, symlink bool
}

func (d fakeDirent) Name() string         { return d.name }
func (d fakeDirent) IsDirectory() bool    { return d.dir }
func (d fakeDirent) IsFile() bool         { return d.file }
func (d fakeDirent) IsSymbolicLink() bool { return d.symlink }

type fakeStat bool

func (f fakeStat) IsDir() bool { return bool(f) }

type dirInfo struct{ fs.FileInfo }

func (dirInfo) IsDir() bool { return true }

// fakeDirFS stands in for the fs implementation a case installs, with no realpath like the doubles
type fakeDirFS struct {
	readdirCalls int
	resolved     string
	entries      []Dirent
	statEntry    func(string) (EntryStat, error)
}

func (f *fakeDirFS) Stat(name string) (fs.FileInfo, error) {
	if name == f.resolved {
		st, _ := os.Stat(os.TempDir())
		return dirInfo{st}, nil
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
}

func (f *fakeDirFS) ReadDir(string) ([]Dirent, error) {
	f.readdirCalls++
	return f.entries, nil
}

func (f *fakeDirFS) StatEntry(name string) (EntryStat, error) { return f.statEntry(name) }

func TestReadDirForIpcHidesNoisyDirectoriesAndFilesFromTheProjectTree(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "node_modules"), 0o755)
	os.Mkdir(filepath.Join(root, "src"), 0o755)
	writeFile(t, filepath.Join(root, "target"), "hidden file")
	writeFile(t, filepath.Join(root, "README.md"), "visible file")
	result := ReadDirForIpc(root, nil)
	if result.Error != "" || !reflect.DeepEqual(entryNames(result), []string{"src", "README.md"}) {
		t.Fatal(result)
	}
}

func TestReadDirForIpcFiltersAHiddenBasenameWhetherItIsAFileOrDirectory(t *testing.T) {
	dirRoot, fileRoot := t.TempDir(), t.TempDir()
	os.Mkdir(filepath.Join(dirRoot, "node_modules"), 0o755)
	writeFile(t, filepath.Join(dirRoot, "visible.txt"), "visible")
	writeFile(t, filepath.Join(fileRoot, "node_modules"), "hidden file")
	writeFile(t, filepath.Join(fileRoot, "visible.txt"), "visible")
	for _, root := range []string{dirRoot, fileRoot} {
		if got := entryNames(ReadDirForIpc(root, nil)); !reflect.DeepEqual(got, []string{"visible.txt"}) {
			t.Fatal(root, got)
		}
	}
}

func TestReadDirForIpcReturnsDirectoriesBeforeFilesAndSortsByNameWithinGroups(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "z.txt"), "z")
	os.Mkdir(filepath.Join(root, "src"), 0o755)
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	os.Mkdir(filepath.Join(root, "lib"), 0o755)
	result := ReadDirForIpc(root, nil)
	if result.Error != "" || !reflect.DeepEqual(entryNames(result), []string{"lib", "src", "a.txt", "z.txt"}) {
		t.Fatal(result)
	}
}

func TestReadDirForIpcAcceptsFileURLsForDirectories(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "src"), 0o755)
	writeFile(t, filepath.Join(root, "README.md"), "visible file")
	result := ReadDirForIpc(pathToFileURL(root), nil)
	if result.Error != "" || !reflect.DeepEqual(entryNames(result), []string{"src", "README.md"}) {
		t.Fatal(result)
	}
}

func TestReadDirForIpcReturnsInvalidPathForBlankOrNonStringInput(t *testing.T) {
	f := &fakeDirFS{}
	for _, input := range []any{"", "   ", nil} {
		got := ReadDirForIpc(input, f)
		if got.Error != "invalid-path" || len(got.Entries) != 0 {
			t.Fatal(input, got)
		}
	}
	if f.readdirCalls != 0 {
		t.Fatal(f.readdirCalls)
	}
}

func TestReadDirForIpcRejectsWindowsDevicePathsBeforeReaddir(t *testing.T) {
	f := &fakeDirFS{}
	got := ReadDirForIpc(`\\?\C:\secret`, f)
	if got.Error != "device-path" || len(got.Entries) != 0 || f.readdirCalls != 0 {
		t.Fatal(got, f.readdirCalls)
	}
}

func TestReadDirForIpcReturnsFilesystemErrorCodesInsteadOfThrowing(t *testing.T) {
	got := ReadDirForIpc(filepath.Join(t.TempDir(), "missing"), nil)
	if got.Error != "ENOENT" || len(got.Entries) != 0 {
		t.Fatal(got)
	}
}

func TestReadDirForIpcMarksASymlinkToADirectoryAsADirectory(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "actual-dir"), 0o755)
	mustSymlink(t, filepath.Join(root, "actual-dir"), filepath.Join(root, "linked-dir"))
	result := ReadDirForIpc(root, nil)
	for _, e := range result.Entries {
		if e.Name == "linked-dir" && e.IsDirectory {
			return
		}
	}
	t.Fatal(result)
}

func TestReadDirForIpcAllowsExpandingSymlinkOrJunctionDirectoriesOutsideTheProjectRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "outside.txt"), "ok")
	link := filepath.Join(root, "outside-link")
	mustSymlink(t, outside, link)
	result := ReadDirForIpc(link, nil)
	want := []DirEntry{{Name: "outside.txt", Path: filepath.Join(link, "outside.txt"), IsDirectory: false}}
	if result.Error != "" || !reflect.DeepEqual(result.Entries, want) {
		t.Fatal(result)
	}
}

func TestReadDirForIpcStatsSymbolicLinksAndUnknownEntriesWithoutDroppingTheWholeListing(t *testing.T) {
	resolved := resolvePath("virtual-root")
	var mu sync.Mutex
	var statCalls []string
	f := &fakeDirFS{resolved: resolved, entries: []Dirent{
		fakeDirent{name: "unknown-entry"}, fakeDirent{name: "linked-dir", symlink: true},
		fakeDirent{name: "broken-link", symlink: true}, fakeDirent{name: "plain.txt", file: true},
	}}
	f.statEntry = func(full string) (EntryStat, error) {
		mu.Lock()
		statCalls = append(statCalls, full)
		mu.Unlock()
		if strings.HasSuffix(full, string(filepath.Separator)+"linked-dir") {
			return fakeStat(true), nil
		}
		return nil, &os.PathError{Op: "stat", Path: full, Err: syscall.ENOENT}
	}
	result := ReadDirForIpc("virtual-root", f)
	sort.Strings(statCalls)
	wantCalls := []string{filepath.Join(resolved, "broken-link"), filepath.Join(resolved, "linked-dir"), filepath.Join(resolved, "unknown-entry")}
	if result.Error != "" || !reflect.DeepEqual(statCalls, wantCalls) {
		t.Fatal(result, statCalls)
	}
	want := []DirEntry{
		{Name: "linked-dir", Path: filepath.Join(resolved, "linked-dir"), IsDirectory: true},
		{Name: "broken-link", Path: filepath.Join(resolved, "broken-link")},
		{Name: "plain.txt", Path: filepath.Join(resolved, "plain.txt")},
		{Name: "unknown-entry", Path: filepath.Join(resolved, "unknown-entry")},
	}
	if !reflect.DeepEqual(result.Entries, want) {
		t.Fatal(result.Entries)
	}
}

func TestReadDirForIpcBoundsConcurrentStatsWhilePreservingCompleteSortedOutput(t *testing.T) {
	resolved := resolvePath("virtual-root")
	var names []string
	for i := 0; i < 105; i++ {
		names = append(names, "entry-"+leftPad(104-i))
	}
	failed := "entry-100"
	dirs := map[string]bool{}
	for i, n := range names {
		if i%10 == 4 && n != failed {
			dirs[n] = true
		}
	}
	entries := []Dirent{fakeDirent{name: "node_modules", symlink: true}}
	for i, n := range names {
		entries = append(entries, fakeDirent{name: n, symlink: i%2 == 0})
	}
	var mu sync.Mutex
	active, peak := 0, 0
	var statCalls []string
	release := make(chan struct{})
	first := make(chan struct{})
	var firstOnce sync.Once
	f := &fakeDirFS{resolved: resolved, entries: entries}
	f.statEntry = func(full string) (EntryStat, error) {
		mu.Lock()
		statCalls = append(statCalls, full)
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		firstOnce.Do(func() { close(first) })
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		name := filepath.Base(full)
		if name == failed {
			return nil, &os.PathError{Op: "stat", Path: full, Err: syscall.ENOENT}
		}
		return fakeStat(dirs[name]), nil
	}
	done := make(chan ReadDirResult)
	go func() { done <- ReadDirForIpc("virtual-root", f) }()
	<-first
	time.Sleep(50 * time.Millisecond)
	close(release)
	result := <-done
	var wantDirs, wantFiles []string
	for _, n := range names {
		if dirs[n] {
			wantDirs = append(wantDirs, n)
		} else {
			wantFiles = append(wantFiles, n)
		}
	}
	sort.Strings(wantDirs)
	sort.Strings(wantFiles)
	if result.Error != "" || len(result.Entries) != len(names) || len(statCalls) != len(names) {
		t.Fatal(result.Error, len(result.Entries), len(statCalls))
	}
	for _, c := range statCalls {
		if strings.HasSuffix(c, string(filepath.Separator)+"node_modules") {
			t.Fatal("stat of hidden entry")
		}
	}
	if peak <= 1 || peak > 16 {
		t.Fatal("peak", peak)
	}
	if !reflect.DeepEqual(entryNames(result), append(wantDirs, wantFiles...)) {
		t.Fatal(entryNames(result))
	}
	count := 0
	for _, e := range result.Entries {
		if e.Name == failed && e.IsDirectory {
			t.Fatal("failed entry is a directory")
		}
		if e.IsDirectory {
			count++
		}
	}
	if count != len(dirs) {
		t.Fatal(count)
	}
}

func leftPad(n int) string {
	s := "000" + jsNumber(float64(n))
	return s[len(s)-3:]
}
