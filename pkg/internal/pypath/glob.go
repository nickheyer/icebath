package pypath

import (
	"os"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python exception a glob pattern raises before any scanning
type PatternError struct {
	Class string
	Msg   string
}

func (e *PatternError) Error() string { return e.Msg }

type globSelect func(path string, exists bool, yield func(string))

type globber struct {
	sep  string
	fold bool
}

// Path(root).glob(pattern) of CPython 3.14 in scandir order, symlinked directories not recursed
func Glob(root, pattern string) ([]string, error) {
	parts, err := parseGlobPattern(pattern)
	if err != nil {
		return nil, err
	}
	g := &globber{sep: pathSep(), fold: windows}
	stack := make([]string, len(parts))
	for i, p := range parts {
		stack[len(parts)-1-i] = p
	}
	sel := g.selector(stack)
	rootStr := PathStr(root)
	last := parts[len(parts)-1]
	anchor, _ := PathParts(rootStr)
	anchorLen := len(anchor)
	var out []string
	sel(Join(rootStr, ""), false, func(p string) {
		if rootStr == "." {
			p = p[min(2, len(p)):]
		}
		switch {
		case last == "":
			p = p[:max(0, len(p)-1)]
		case last == "**":
			if len(p) > anchorLen && strings.HasSuffix(p, g.sep) {
				p = p[:len(p)-1]
			}
		}
		out = append(out, PathStr(p))
	})
	return out, nil
}

// pathlib _parse_pattern, relative parts with a final empty part for a trailing separator
func parseGlobPattern(pattern string) ([]string, error) {
	drive, root, rel := "", "", pattern
	if windows {
		drive, root, rel = NtSplitRoot(pattern)
		rel = strings.ReplaceAll(rel, "/", `\`)
	} else if strings.HasPrefix(pattern, "/") {
		root, rel = "/", strings.TrimLeft(pattern, "/")
	}
	if root != "" || drive != "" {
		return nil, &PatternError{Class: "NotImplementedError", Msg: "Non-relative patterns are unsupported"}
	}
	sep := pathSep()
	var parts []string
	for _, x := range strings.Split(rel, sep) {
		if x != "" && x != "." {
			parts = append(parts, x)
		}
	}
	if len(parts) == 0 {
		return nil, &PatternError{Class: "ValueError", Msg: "Unacceptable pattern: " + pyrepr.StrRepr(pattern)}
	}
	if strings.HasSuffix(rel, sep) {
		parts = append(parts, "")
	}
	return parts, nil
}

func hasGlobMagic(s string) bool { return strings.ContainsAny(s, "*?[") }

func isSpecialGlobPart(s string) bool { return s == "" || s == "." || s == ".." }

// Selector for the reversed pattern parts, consuming them from the end like _GlobberBase.selector
func (g *globber) selector(parts []string) globSelect {
	if len(parts) == 0 {
		return g.selectExists
	}
	part := parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	switch {
	case part == "**":
		return g.recursiveSelector(parts)
	case isSpecialGlobPart(part):
		if len(parts) > 0 {
			part += g.sep
		}
		next := g.selector(parts)
		return func(path string, exists bool, yield func(string)) { next(path+part, exists, yield) }
	case !hasGlobMagic(part):
		for len(parts) > 0 && !hasGlobMagic(parts[len(parts)-1]) {
			part += g.sep + parts[len(parts)-1]
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 0 {
			part += g.sep
		}
		next := g.selector(parts)
		return func(path string, _ bool, yield func(string)) { next(path+part, false, yield) }
	}
	return g.wildcardSelector(part, parts)
}

// Compiled single segment pattern, case folded where the host path flavour ignores case
func (g *globber) compile(part string) *regexp.Regexp {
	re := fnmatch.Compile(part)
	if g.fold {
		re = regexp.MustCompile("(?i)" + re.String())
	}
	return re
}

// Entries of a directory in the order the OS lists them, an unreadable directory listing nothing
func globScandir(path string) []os.DirEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil
	}
	return entries
}

func (g *globber) wildcardSelector(part string, parts []string) globSelect {
	var match *regexp.Regexp
	if part != "*" {
		match = g.compile(part)
	}
	dirOnly := len(parts) > 0
	var next globSelect
	if dirOnly {
		next = g.selector(parts)
	}
	return func(path string, _ bool, yield func(string)) {
		for _, e := range globScandir(path) {
			if match != nil && !match.MatchString(e.Name()) {
				continue
			}
			entryPath := Join(path, e.Name())
			if !dirOnly {
				yield(entryPath)
				continue
			}
			if st, err := os.Stat(entryPath); err != nil || !st.IsDir() {
				continue
			}
			next(entryPath+g.sep, true, yield)
		}
	}
}

// Double star over every directory below the path without following symlinks, depth first from a stack
func (g *globber) recursiveSelector(parts []string) globSelect {
	for len(parts) > 0 && parts[len(parts)-1] == "**" {
		parts = parts[:len(parts)-1]
	}
	dirOnly := len(parts) > 0
	next := g.selector(parts)
	return func(path string, exists bool, yield func(string)) {
		next(path, exists, yield)
		stack := []string{path}
		for len(stack) > 0 {
			dir := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, e := range globScandir(dir) {
				isDir := e.IsDir()
				if !isDir && dirOnly {
					continue
				}
				entryPath := Join(dir, e.Name())
				if dirOnly {
					entryPath += g.sep
					next(entryPath, true, yield)
				} else {
					yield(entryPath)
				}
				if isDir {
					stack = append(stack, entryPath)
				}
			}
		}
	}
}

func (g *globber) selectExists(path string, exists bool, yield func(string)) {
	if exists {
		yield(path)
		return
	}
	if _, err := os.Lstat(path); err == nil {
		yield(path)
	}
}
