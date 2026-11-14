package approval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
)

type protectedPath struct {
	path        string
	description string
}

type Protector struct {
	entries []protectedPath
}

var (
	runtimeSegmentSplitRe = regexp.MustCompile(`\n|&&|\|\||[;|]`)
	commandPrefixes       = set("sudo", "command", "nohup", "exec", "env", "nice", "time")
	deletingCommands      = set("rm", "rmdir", "rd", "del", "erase", "remove-item", "ri")
)

func NewProtector() (*Protector, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the running executable: %w", err)
	}
	root, err := home.InstallRoot()
	if err != nil {
		return nil, fmt.Errorf("locate the install directory: %w", err)
	}
	p := &Protector{}
	binary := normalizeRuntimePath(exe)
	if binary != "" {
		p.entries = append(p.entries, protectedPath{binary, "the icebath binary this runtime is running from"})
	}
	if dir := normalizeRuntimePath(root); dir != "" && dir != binary {
		p.entries = append(p.entries, protectedPath{dir, "the install directory this icebath runtime is running from"})
	}
	return p, nil
}

func normalizeRuntimePath(raw string) string {
	path := pyStrip(raw)
	if path == "" {
		return ""
	}
	if r := []rune(path); len(r) >= 2 && r[0] == r[len(r)-1] && (r[0] == '"' || r[0] == '\'') {
		path = string(r[1 : len(r)-1])
	}
	path = driveForm(expandVars(home.ExpandUser(path)))
	return normcase(realpath(filepath.Clean(path)))
}

func overlaps(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+pathSep) || strings.HasPrefix(b, a+pathSep)
}

func SplitEntry(path string) (string, string) {
	drive, tail := splitDrive(path)
	trimmed := strings.TrimRight(tail, sepChars)
	if trimmed == "" && tail != "" {
		trimmed = tail[:1]
	}
	parent, leaf := splitPath(drive + trimmed)
	if leaf == "." && parent != "" {
		return SplitEntry(parent)
	}
	return parent, leaf
}

func plainJoin(a, b string) string {
	switch {
	case b != "" && strings.ContainsRune(sepChars, rune(b[0])):
		return b
	case a == "" || strings.ContainsRune(sepChars, rune(a[len(a)-1])):
		return a + b
	}
	return a + pathSep + b
}

func (p *Protector) Path(path string, follow bool) (string, bool) {
	var resolved string
	if follow {
		resolved = normalizeRuntimePath(path)
	} else {
		parent, leaf := SplitEntry(path)
		if parent == "" {
			parent = "."
		}
		resolved = normalizeRuntimePath(parent)
		if resolved != "" {
			resolved = normcase(plainJoin(resolved, leaf))
		}
	}
	if resolved == "" {
		return "", false
	}
	for _, e := range p.entries {
		if overlaps(resolved, e.path) {
			return e.description, true
		}
	}
	return "", false
}

func runtimeWords(segment string) []string {
	words, err := shell.Split(segment)
	if err != nil {
		return pyFields(segment)
	}
	return words
}

func stripCommandPrefixes(words []string) []string {
	for len(words) > 0 {
		first := words[0]
		if commandPrefixes[first] {
			words = words[1:]
			continue
		}
		if strings.Contains(first, "=") && !strings.HasPrefix(first, "-") && !strings.HasPrefix(first, "/") && envAssignmentPrefix(first) {
			words = words[1:]
			continue
		}
		break
	}
	return words
}

func findDeletes(segment string) bool {
	for i := 0; i < len(segment); i++ {
		if segment[i] != '-' || (i > 0 && !pySpace(runeBefore(segment, i))) {
			continue
		}
		rest := segment[i+1:]
		if _, ok := foldPrefix(rest, "delete"); ok {
			return true
		}
		if n, ok := foldPrefix(rest, "exec"); ok {
			j := i + 1 + n
			k := skipSpace(segment, j)
			if k > j {
				if m, ok := foldPrefix(segment[k:], "rm"); ok && !wordAt(segment, k+m) {
					return true
				}
			}
		}
	}
	return false
}

func (p *Protector) Command(command string) (string, bool) {
	if pyStrip(command) == "" || len(p.entries) == 0 {
		return "", false
	}
	for _, segment := range runtimeSegmentSplitRe.Split(command, -1) {
		words := stripCommandPrefixes(runtimeWords(segment))
		if len(words) == 0 {
			continue
		}
		name := pyLower(pyBasename(words[0]))
		if name == "find" && findDeletes(segment) {
			for _, word := range words[1:] {
				if !strings.HasPrefix(word, "-") {
					if hit, ok := p.Path(word, true); ok {
						return hit, true
					}
				}
			}
			continue
		}
		if !deletingCommands[name] {
			continue
		}
		for _, word := range words[1:] {
			if strings.HasPrefix(word, "-") {
				continue
			}
			if hit, ok := p.Path(word, true); ok {
				return hit, true
			}
		}
	}
	return "", false
}
