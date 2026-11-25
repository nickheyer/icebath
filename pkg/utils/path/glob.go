package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/utils/fnmatch"
)

// Pattern walk state: the pattern elements, whether only directories may match, and the matches found
type globber struct {
	elems   []string
	dirOnly bool
	fold    bool
	seen    map[string]bool
	out     []string
}

// Paths below root matching a relative pattern of shell wildcard elements, ** standing for any depth of directories without entering symlinked ones and a trailing separator for directories only, in directory order
func Glob(root, pattern string) ([]string, error) {
	g, err := parseGlob(pattern)
	if err != nil {
		return nil, err
	}
	g.walk(filepath.Clean(root), 0, false)
	return g.out, nil
}

func parseGlob(pattern string) (*globber, error) {
	isSep := func(r rune) bool { return r == '/' }
	absolute := strings.HasPrefix(pattern, "/")
	if runtime.GOOS == "windows" {
		isSep = func(r rune) bool { return r == '/' || r == '\\' }
		drive, root, _ := WindowsSplitRoot(pattern)
		absolute = drive != "" || root != ""
	}
	if absolute {
		return nil, fmt.Errorf("glob pattern %q is not relative: %w", pattern, filepath.ErrBadPattern)
	}
	g := &globber{fold: runtime.GOOS == "windows"}
	// Several ** elements, or a dot-dot after a wildcard folding the matches back onto one path, can reach a path twice
	stars, wild, repeats := 0, false, false
	for elem := range strings.FieldsFuncSeq(pattern, isSep) {
		switch {
		case elem == ".":
		case elem == "**" && len(g.elems) > 0 && g.elems[len(g.elems)-1] == "**":
		default:
			if elem == "**" {
				stars++
			}
			repeats = repeats || elem == ".." && wild
			wild = wild || strings.ContainsAny(elem, "*?[")
			g.elems = append(g.elems, elem)
		}
	}
	if len(g.elems) == 0 {
		return nil, fmt.Errorf("glob pattern %q names nothing: %w", pattern, filepath.ErrBadPattern)
	}
	last := pattern[len(pattern)-1]
	g.dirOnly = last == '/' || runtime.GOOS == "windows" && last == '\\'
	if stars > 1 || repeats {
		g.seen = map[string]bool{}
	}
	return g, nil
}

func (g *globber) yield(p string) {
	if g.seen != nil {
		if g.seen[p] {
			return
		}
		g.seen[p] = true
	}
	g.out = append(g.out, p)
}

// Matcher of one wildcard element, nil for a lone star that matches every name
func (g *globber) matcher(elem string) *regexp.Regexp {
	if elem == "*" {
		return nil
	}
	if g.fold {
		return fnmatch.Compile(strings.ToLower(elem))
	}
	return fnmatch.Compile(elem)
}

func (g *globber) matches(re *regexp.Regexp, name string) bool {
	if re == nil {
		return true
	}
	if g.fold {
		name = strings.ToLower(name)
	}
	return re.MatchString(name)
}

// Matches of the pattern elements from i on below dir, which is known to exist when exists is set
func (g *globber) walk(dir string, i int, exists bool) {
	if i == len(g.elems) {
		if !exists {
			fi, err := os.Lstat(dir)
			if err != nil || g.dirOnly && !isDirFollowed(dir, fi) {
				return
			}
		}
		g.yield(dir)
		return
	}
	elem := g.elems[i]
	switch {
	case elem == "**":
		g.walkRecursive(dir, i+1, exists)
	case !strings.ContainsAny(elem, "*?["):
		g.walk(filepath.Join(dir, elem), i+1, false)
	default:
		re := g.matcher(elem)
		final := i == len(g.elems)-1
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !g.matches(re, e.Name()) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if final && !g.dirOnly {
				g.yield(p)
				continue
			}
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				continue
			}
			g.walk(p, i+1, true)
		}
	}
}

// Matches of the elements from i on in dir and in every directory below it reached without following symlinks
func (g *globber) walkRecursive(dir string, i int, exists bool) {
	final := i == len(g.elems)
	g.walk(dir, i, exists)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			g.walkRecursive(p, i, true)
		case final && !g.dirOnly:
			g.yield(p)
		}
	}
}

// Whether a looked up path is a directory, following it when it is a symlink
func isDirFollowed(p string, fi os.FileInfo) bool {
	if fi.Mode()&os.ModeSymlink != 0 {
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	}
	return fi.IsDir()
}
