package plugin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Names an instruction file may have, in the order one directory is searched
var InstructionNames = []string{"ICEBATH.md", "AGENTS.md", "CLAUDE.md"}

// Local instruction files read beside the main one, kept out of version control
var LocalInstructionNames = []string{"ICEBATH.local.md", "CLAUDE.local.md"}

// Depth of imports followed inside instruction files
const importDepth = 5

// Why an instruction file was loaded, as hooks hear it
const (
	LoadSessionStart = "session_start"
	LoadNested       = "nested_traversal"
	LoadPathGlob     = "path_glob_match"
	LoadInclude      = "include"
	LoadCompact      = "compact"
)

// Scopes an instruction file belongs to
const (
	MemoryUser    = "User"
	MemoryProject = "Project"
	MemoryLocal   = "Local"
	MemoryManaged = "Managed"
)

// One instruction file as loaded, with the rules under it that apply only to some files
type Instruction struct {
	Path string
	// User, Project, Local or Managed
	Scope string
	// Text with imports resolved
	Text string
	// Globs the text applies to, empty for always
	Paths []string
	// File whose import brought this one in, empty for a file read directly
	Parent string
	// Why the file was loaded
	Reason string
}

// Reports whether the instruction applies always rather than to matching files only
func (in Instruction) Always() bool { return len(in.Paths) == 0 }

var importRef = regexp.MustCompile(`(^|\s)@([^\s@,;:'"<>()]+)`)

// Loads the instruction files of a session, managed, user, the project walk and their rule directories
func LoadInstructions(configDir, root, dir string) []Instruction {
	var out []Instruction
	seen := map[string]bool{}
	add := func(path, scope, reason string, paths []string) {
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			return
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		seen[abs] = true
		text, front := splitRule(string(data))
		if front != nil {
			paths = append(paths, front...)
		}
		in := Instruction{Path: abs, Scope: scope, Paths: paths, Reason: reason}
		in.Text = resolveImports(abs, text, 0, seen, &out, scope)
		out = append(out, in)
	}
	if isDir(managedDir) {
		for _, name := range []string{"CLAUDE.md", "ICEBATH.md"} {
			add(filepath.Join(managedDir, name), MemoryManaged, LoadSessionStart, nil)
		}
		addRules(filepath.Join(managedDir, ".claude", "rules"), MemoryManaged, add)
	}
	if configDir != "" {
		for _, name := range InstructionNames {
			add(filepath.Join(configDir, name), MemoryUser, LoadSessionStart, nil)
		}
		addRules(filepath.Join(configDir, "rules"), MemoryUser, add)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude", "CLAUDE.md"), MemoryUser, LoadSessionStart, nil)
		addRules(filepath.Join(home, ".claude", "rules"), MemoryUser, add)
		add(filepath.Join(home, ".codex", "AGENTS.md"), MemoryUser, LoadSessionStart, nil)
	}
	for i, d := range lineage(root, dir) {
		reason := LoadSessionStart
		if i > 0 {
			reason = LoadNested
		}
		for _, name := range InstructionNames {
			if exists(filepath.Join(d, name)) {
				add(filepath.Join(d, name), MemoryProject, reason, nil)
				break
			}
		}
		for _, name := range LocalInstructionNames {
			add(filepath.Join(d, name), MemoryLocal, reason, nil)
		}
		for _, sub := range []string{".icebath", ".claude"} {
			if exists(filepath.Join(d, sub, "CLAUDE.md")) {
				add(filepath.Join(d, sub, "CLAUDE.md"), MemoryProject, reason, nil)
			}
			addRules(filepath.Join(d, sub, "rules"), MemoryProject, add)
		}
	}
	return out
}

// Adds every markdown file under a rules directory, walking it
func addRules(dir, scope string, add func(path, scope, reason string, paths []string)) {
	var files []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(path) == ".md" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	for _, f := range files {
		add(f, scope, LoadSessionStart, nil)
	}
}

// Splits a rule file into its body and the paths of its frontmatter, nil paths for a file without
func splitRule(text string) (string, []string) {
	f := ParseFront(text)
	if !f.Present || f.Err != nil {
		return text, nil
	}
	paths := f.List("paths")
	if paths == nil {
		paths = []string{}
	}
	return f.Body, paths
}

// Replaces @path references in the text with the contents of the files, up to the import depth
func resolveImports(file, text string, depth int, seen map[string]bool, out *[]Instruction, scope string) string {
	if depth >= importDepth {
		return text
	}
	base := filepath.Dir(file)
	return importRef.ReplaceAllStringFunc(text, func(m string) string {
		parts := importRef.FindStringSubmatch(m)
		ref := parts[2]
		if strings.Contains(ref, "://") || strings.HasPrefix(ref, "agent-") {
			return m
		}
		path := expandHome(ref)
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return m
		}
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			return m
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return m
		}
		if !seen[abs] {
			seen[abs] = true
			*out = append(*out, Instruction{Path: abs, Scope: scope, Parent: file, Reason: LoadInclude, Text: string(data)})
		}
		body := resolveImports(abs, string(data), depth+1, seen, out, scope)
		return parts[1] + "<!-- imported from " + ref + " -->\n" + strings.TrimSpace(body) + "\n"
	})
}

// Instructions that always apply, in load order
func Always(list []Instruction) []Instruction {
	var out []Instruction
	for _, in := range list {
		if in.Always() && in.Reason != LoadInclude {
			out = append(out, in)
		}
	}
	return out
}

// Instructions whose globs match the file, relative to the working directory
func Matching(list []Instruction, dir, path string) []Instruction {
	var out []Instruction
	for _, in := range list {
		if !in.Always() && matchesAny(in.Paths, dir, path) {
			out = append(out, in)
		}
	}
	return out
}
