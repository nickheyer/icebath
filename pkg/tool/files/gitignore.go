package files

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/glob"
)

// One line of a .gitignore file
type ignoreRule struct {
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
}

// Rules of one .gitignore file applying to everything below its directory
type ignoreFrame struct {
	dir   string
	rules []ignoreRule
}

// Every .gitignore met so far, outer directories first
type ignoreStack struct {
	frames []ignoreFrame
}

// Starts a stack with the .gitignore of the walk root when it has one
func loadIgnoreStack(root string) ignoreStack {
	var s ignoreStack
	s.push(root)
	return s
}

// Adds the .gitignore of dir when present
func (s *ignoreStack) push(dir string) {
	rules := parseIgnore(filepath.Join(dir, ".gitignore"))
	if len(rules) > 0 {
		s.frames = append(s.frames, ignoreFrame{dir: dir, rules: rules})
	}
}

// Reports whether p is hidden by the last matching rule of any frame above it
func (s ignoreStack) ignored(p string, isDir bool) bool {
	ignored := false
	for _, f := range s.frames {
		rel, err := filepath.Rel(f.dir, p)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		rel = filepath.ToSlash(rel)
		for _, r := range f.rules {
			if r.dirOnly && !isDir {
				continue
			}
			if r.matches(rel) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}

// Matches an anchored pattern against the path or an ancestor and a bare one against any segment
func (r ignoreRule) matches(rel string) bool {
	if r.anchored {
		for _, prefix := range prefixes(rel) {
			if glob.Match(r.pattern, prefix) {
				return true
			}
		}
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if glob.Match(r.pattern, seg) {
			return true
		}
	}
	return false
}

// Every leading sub path of rel including rel itself
func prefixes(rel string) []string {
	segs := strings.Split(rel, "/")
	out := make([]string, 0, len(segs))
	for i := range segs {
		out = append(out, strings.Join(segs[:i+1], "/"))
	}
	return out
}

// Reads the rules of one .gitignore file, empty when it is missing or unreadable
func parseIgnore(path string) []ignoreRule {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []ignoreRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if r, ok := parseIgnoreLine(sc.Text()); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

// Parses one .gitignore line, reporting false for blanks and comments
func parseIgnoreLine(line string) (ignoreRule, bool) {
	line = strings.TrimRight(line, "\r")
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
		line = strings.TrimSuffix(line, " ")
	}
	line = strings.ReplaceAll(line, "\\ ", " ")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var r ignoreRule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	line = strings.TrimPrefix(line, "\\")
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		r.anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(line, "/") {
		r.anchored = true
	}
	if line == "" {
		return ignoreRule{}, false
	}
	r.pattern = line
	return r, true
}
