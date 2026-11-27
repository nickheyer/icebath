package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Prefix of trees a publish could not delete, reclaimed by the next install or gc
const setAside = ".reclaim-"

// One folder of published entries, the archives they came from and a scratch area
type store struct {
	root     string
	partials string
}

func (h *Packages) store() *store {
	return &store{root: h.storeRoot(), partials: h.partials()}
}

func (s *store) entry(name string) string { return filepath.Join(s.root, name) }

// Fresh staging folder inside the store and its removal
func (s *store) scratch() (string, func(), error) {
	if err := os.MkdirAll(s.root, 0o777); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(s.root, ".staging-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { removeQuiet(dir) }, nil
}

// Renames a staged tree into place, keeping the entry a concurrent install published first
func (s *store) publish(staged, name string) error {
	target := s.entry(name)
	delay := 500 * time.Millisecond
	for range 5 {
		if err := os.Rename(staged, target); err == nil || fsx.IsDir(target) {
			return nil
		}
		time.Sleep(delay)
		delay *= 2
	}
	if err := os.Rename(staged, target); err != nil && !fsx.IsDir(target) {
		return err
	}
	return nil
}

// Takes the store's install lock, telling the person what it waits on after two seconds
func (s *store) lock(r Reporter) (func(), error) {
	path := filepath.Join(s.root, ".install.lock")
	release, err := fsx.Lock(context.Background(), path, 2*time.Second)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return release, err
	}
	reporter(r).Line(fmt.Sprintf("waiting for %s (another PM operation holds it)", path))
	return fsx.Lock(context.Background(), path, -1)
}

// Fetches pinned archives into the hash keyed cache with one progress for all of them, finished ones kept even when a later one stops
func (s *store) fetch(ctx context.Context, artifacts Archives, scratch string, progress Progress) ([]string, error) {
	var sources []Source
	for _, a := range artifacts {
		if err := paused(ctx); err != nil {
			return nil, err
		}
		entry := s.entry("fetch-" + a.SHA256)
		dest := ""
		if items, err := os.ReadDir(entry); err == nil && len(items) == 1 && fsx.IsFile(filepath.Join(entry, items[0].Name())) {
			dest = filepath.Join(entry, items[0].Name())
		} else {
			if fsx.Lexists(entry) {
				if err := removeTree(entry); err != nil {
					return nil, err
				}
			}
			dest = filepath.Join(scratch, "fetch-"+a.SHA256, a.URL[strings.LastIndex(a.URL, "/")+1:])
		}
		source, err := pinnedSource(a.URL, dest, a.SHA256)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	byDest := map[string]string{}
	for i, source := range sources {
		byDest[source.Dest] = artifacts[i].URL
	}
	tick := func(done, total int64, coverage []Coverage) error {
		if progress == nil {
			return nil
		}
		mapped := make([]Coverage, len(coverage))
		for i, c := range coverage {
			mapped[i] = Coverage{Key: byDest[c.Key], Ranges: c.Ranges}
		}
		return progress(done, total, mapped)
	}
	_, runErr := (&Download{Sources: sources, Partials: s.partials}).Run(ctx, tick)
	for _, source := range sources {
		if fsx.Within(source.Dest, scratch) && fsx.IsFile(source.Dest) {
			if err := s.publish(filepath.Dir(source.Dest), "fetch-"+source.SHA256); err != nil {
				return nil, err
			}
		}
	}
	if runErr != nil {
		return nil, runErr
	}
	out := make([]string, len(sources))
	for i, source := range sources {
		out[i] = filepath.Join(s.entry("fetch-"+source.SHA256), filepath.Base(source.Dest))
	}
	return out, nil
}

// Deletes an entry, waiting out short holds, failing when its bytes survive every try
func (s *store) remove(name string, attempts int) error {
	entry := s.entry(name)
	for attempt := range attempts {
		st, err := os.Lstat(entry)
		if err != nil {
			return nil
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			err = os.Remove(entry)
		} else {
			err = os.RemoveAll(entry)
		}
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if attempt == attempts-1 {
			return err
		}
		time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
	}
	return nil
}

// Drops leftovers, renaming a tree still held aside so its place frees for the next publish
func (s *store) discard(name string) {
	err := s.remove(name, 5)
	if err == nil {
		return
	}
	if rerr := os.Rename(s.entry(name), s.entry(setAside+strs.Random(16))); rerr != nil {
		slog.Warn(fmt.Sprintf("could not remove or set aside %s: %s", name, err))
		return
	}
	slog.Warn(fmt.Sprintf("%s is still in use (%s); set aside for the next install or `icebath pm gc`", name, err))
}

// Deletes the set aside trees nothing holds any more and counts them
func (s *store) reclaim() int {
	items, _ := os.ReadDir(s.root)
	removed := 0
	for _, item := range items {
		if strings.HasPrefix(item.Name(), setAside) && s.remove(item.Name(), 1) == nil {
			removed++
		}
	}
	return removed
}

// Digest of a tree from its sorted relative paths and contents, links by their text, desktop metadata and bytecode caches left out
func treeDigest(root string) (string, error) {
	type item struct{ rel, path string }
	var items []item
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		link := d.Type()&fs.ModeSymlink != 0 || junction(path)
		switch {
		case link:
			items = append(items, item{filepath.ToSlash(rel), path})
			return nil
		case d.IsDir() && d.Name() == "__pycache__":
			return fs.SkipDir
		case d.IsDir():
			return nil
		case metadata(path):
			return nil
		}
		items = append(items, item{filepath.ToSlash(rel), path})
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.rel, b.rel) })
	h := sha256.New()
	buf := make([]byte, chunk)
	for _, it := range items {
		h.Write([]byte(it.rel))
		h.Write([]byte{0})
		if st, err := os.Lstat(it.path); err == nil && (st.Mode()&fs.ModeSymlink != 0 || junction(it.path)) {
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
		_, err = io.CopyBuffer(h, f, buf)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
