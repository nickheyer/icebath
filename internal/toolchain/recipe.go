package toolchain

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

// What a catalog entry gives its consumer
type Kind string

// A runnable program, a shared library loaded in process, and files read as data
const (
	Program Kind = "program"
	Library Kind = "library"
	Data    Kind = "data"
)

// Definition of one catalog entry, everything about it except its pinned version and digests
type recipe struct {
	name     string
	kind     Kind
	required bool
	standard bool
	onPath   bool
	gap      func(Target) string
	entry    map[string]string
	find     func(dir string, target Target) []string
	marker   string
	files    []string
	probe    []string
	probeDir bool
	probeEnv func(ctx context.Context, s *Store) []string
	emulated map[Target]bool
	flatten  bool
	dirName  func(version string, target Target) string
	unpack   func(ctx context.Context, st *staging, index int, archive, dest string) (merge bool, err error)
	stage    func(ctx context.Context, st *staging) error
	env      func(dir string, target Target) map[string][]string
	homeDir  home.Entry
	partials home.Entry
	raw      bool
	present  string
}

// One install in progress, the staged tree and the pins it is built from
type staging struct {
	store     *Store
	recipe    *recipe
	target    Target
	version   string
	tree      string
	scratch   string
	artifacts []Artifact
}

// Reason upstream ships no build of the entry for target, empty when one exists
func (r *recipe) missing(target Target) string {
	if r.gap == nil {
		return ""
	}
	return r.gap(target)
}

// Folder name an install of version for target is published under
func (r *recipe) directory(version string, target Target) string {
	if r.dirName != nil {
		return r.dirName(version, target)
	}
	return r.name + "-" + version + "-" + string(target)
}

// Reports an entry kept in a profile home instead of the shared store
func (r *recipe) inHome() bool { return r.homeDir != "" }

// Entry points of an install directory for target with the pattern that names them
func (r *recipe) binaries(dir string, target Target) ([]string, string) {
	if r.find != nil {
		return r.find(dir, target), ""
	}
	pattern, ok := lookupTarget(r.entry, target)
	if !ok || pattern == "" {
		return nil, ""
	}
	return entryMatches(dir, pattern, target.bionic()), pattern
}

// First entry point of an install directory for target, empty when the entry has none there
func (r *recipe) binary(dir string, target Target) string {
	found, _ := r.binaries(dir, target)
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// Environment an install contributes, PATH holding the entry point directory of an entry on the search path
func (r *recipe) environment(dir string, target Target) map[string][]string {
	if r.env != nil {
		return r.env(dir, target)
	}
	if !r.onPath {
		return map[string][]string{}
	}
	if binary := r.binary(dir, target); binary != "" {
		return map[string][]string{"PATH": {filepath.Dir(binary)}}
	}
	return map[string][]string{}
}

// Gap function answering reason for every target the predicate rejects
func gapUnless(reason string, has func(Target) bool) func(Target) string {
	return func(t Target) string {
		if has(t) {
			return ""
		}
		return reason
	}
}

// Gap function answering the reason recorded for a target
func gapTable(table map[Target]string) func(Target) string {
	return func(t Target) string { return table[t] }
}

// Regular files below dir whose base name is one of names, in path order
func filesNamed(dir string, names ...string) []string {
	var found []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		for _, name := range names {
			if d.Name() == name {
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
					found = append(found, p)
				}
			}
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool {
		a, b := strings.Split(found[i], string(filepath.Separator)), strings.Split(found[j], string(filepath.Separator))
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return found
}

// Reports a file name that is release provenance rather than an archive
func provenanceName(name string) bool {
	for _, suffix := range []string{".txt", ".sig", ".pem", ".asc"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
