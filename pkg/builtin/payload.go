package builtin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Embedded plugin projects, one directory per project
//
//go:embed all:payload
var payloadFS embed.FS

// Bumped whenever the derived host layout written beside the payload changes shape
const hostLayoutVersion = "2"

// One embedded file with its digest and materialized mode
type payloadEntry struct {
	Path string
	Sum  string
	Mode fs.FileMode
}

// One materialized file, its contents and permission bits
type treeFile struct {
	Data []byte
	Mode fs.FileMode
}

// Materialized builtin plugin tree under the root home cache
type Tree struct {
	Root string
}

// Absolute path of an embedded payload file, given relative to the references root
func (t *Tree) Payload(rel string) string {
	return filepath.Join(t.Root, "payload", filepath.FromSlash(rel))
}

// Absolute path inside the derived host layout written beside the payload
func (t *Tree) Host(rel string) string {
	return filepath.Join(t.Root, "host", filepath.FromSlash(rel))
}

var (
	payloadEntriesOnce sync.Once
	payloadEntryList   []payloadEntry
	payloadEntryErr    error
)

// Every embedded payload file in path order with its digest and mode, scripts being executable
func manifestEntries() ([]payloadEntry, error) {
	payloadEntriesOnce.Do(func() {
		payloadEntryErr = fs.WalkDir(payloadFS, "payload", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := payloadFS.ReadFile(p)
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if strings.HasPrefix(string(data[:min(len(data), 2)]), "#!") {
				mode = 0o755
			}
			sum := sha256.Sum256(data)
			payloadEntryList = append(payloadEntryList, payloadEntry{Path: strings.TrimPrefix(p, "payload/"), Sum: hex.EncodeToString(sum[:]), Mode: mode})
			return nil
		})
		sort.Slice(payloadEntryList, func(i, j int) bool { return payloadEntryList[i].Path < payloadEntryList[j].Path })
	})
	return payloadEntryList, payloadEntryErr
}

// Embedded bytes of a payload file given relative to the references root
func payloadFile(rel string) ([]byte, error) {
	return payloadFS.ReadFile(path.Join("payload", rel))
}

// Sorted payload paths under a directory prefix given relative to the references root
func payloadUnder(prefix string) []string {
	var out []string
	_ = fs.WalkDir(payloadFS, path.Join("payload", prefix), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, strings.TrimPrefix(p, "payload/"))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Sixteen hex digits naming the payload, the host layout version and the running icebath version
func payloadDigest() string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%s\x00", plugins.RunningVersion, hostLayoutVersion)
	entries, _ := manifestEntries()
	for _, e := range entries {
		fmt.Fprintf(sum, "%s %s %04o\n", e.Path, e.Sum, e.Mode)
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

var (
	materializeMu sync.Mutex
	materialized  = map[string]*Tree{}
)

// Writes the payload and host layout once per content digest under the root home cache
func Materialize() (*Tree, error) {
	return materializeAt(home.CacheDir(home.Root()))
}

// Writes the tree under a cache root once per process and digest, reusing a completed directory
func materializeAt(cacheRoot string) (*Tree, error) {
	dir := filepath.Join(cacheRoot, "builtin-plugins", plugins.RunningVersion+"-"+payloadDigest())
	materializeMu.Lock()
	defer materializeMu.Unlock()
	if t, ok := materialized[dir]; ok && isFile(filepath.Join(dir, ".complete")) {
		return t, nil
	}
	t, err := writeTree(dir)
	if err != nil {
		return nil, err
	}
	materialized[dir] = t
	return t, nil
}

// Builds the tree in a private sibling directory and renames it into place, a racing winner being kept
func writeTree(dir string) (*Tree, error) {
	marker := filepath.Join(dir, ".complete")
	t := &Tree{Root: dir}
	if isFile(marker) {
		return t, nil
	}
	files, err := treeFiles(t)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, files[name].Data, files[name].Mode); err != nil {
			return nil, err
		}
		if err := os.Chmod(target, files[name].Mode); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), []byte(filepath.Base(dir)), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if isFile(marker) {
			return t, nil
		}
		return nil, err
	}
	return t, nil
}

// Every file of the materialized tree keyed by its slash path below the tree root
func treeFiles(t *Tree) (map[string]treeFile, error) {
	entries, err := manifestEntries()
	if err != nil {
		return nil, err
	}
	files := map[string]treeFile{}
	for _, e := range entries {
		data, err := payloadFile(e.Path)
		if err != nil {
			return nil, err
		}
		files["payload/"+e.Path] = treeFile{Data: data, Mode: e.Mode}
	}
	for _, layout := range []func(*Tree, map[string]treeFile) error{egmHostLayout, impeccableHostLayout} {
		if err := layout(t, files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// Copies every payload file under a prefix into the tree at a destination directory
func copyPayloadTree(files map[string]treeFile, prefix, dest string) {
	for _, rel := range payloadUnder(prefix) {
		src := files["payload/"+rel]
		files[path.Join(dest, strings.TrimPrefix(rel, prefix+"/"))] = src
	}
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
