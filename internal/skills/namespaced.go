package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Shape of a namespace and of a namespaced skill's name
var namespaceName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// One skill of a namespaced source
type Plugin struct {
	// Name of the source, the part before the colon
	Namespace string
	// Name of the skill's folder, the part after it
	Name string
	// Its SKILL.md on disk
	Path        string
	Description string
	Frontmatter Frontmatter
}

// Qualified name, namespace:skill
func (p Plugin) Qualified() string {
	return p.Namespace + ":" + p.Name
}

// Folder a namespaced source is served from: its own on disk, else a copy of the binary's under the config folder, written again when
// the files differ
func (s *Scope) served(source Source) (string, error) {
	if source.Dir != "" {
		return source.Dir, nil
	}
	dir := filepath.Join(s.Config.Dir(), "plugin-skills", source.Name)
	digest, err := treeDigest(source.Files)
	if err != nil {
		return "", err
	}
	st := s.registry.shared()
	st.mu.Lock()
	done := st.materialized[source.Name] == digest
	st.mu.Unlock()
	if done && fsx.IsDir(dir) {
		return dir, nil
	}
	stamp := filepath.Join(dir, ".digest")
	if kept, err := os.ReadFile(stamp); err != nil || string(kept) != digest {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := copyTree(source.Files, ".", dir, false, false); err != nil {
			return "", err
		}
		if err := os.WriteFile(stamp, []byte(digest), 0o644); err != nil {
			return "", err
		}
	}
	st.mu.Lock()
	if st.materialized == nil {
		st.materialized = map[string]string{}
	}
	st.materialized[source.Name] = digest
	st.mu.Unlock()
	return dir, nil
}

// Digest of every path and file of a tree
func treeDigest(files fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// Every skill of the namespaced sources, by qualified name, sources registered first winning a name
func (s *Scope) Plugins() []Plugin {
	var out []Plugin
	for _, source := range s.registry.kind(s.Config, Namespaced) {
		if !namespaceName.MatchString(source.Name) {
			continue
		}
		dir, err := s.served(source)
		if err != nil {
			continue
		}
		for _, skillMD := range skillFiles(dir) {
			bare := filepath.Base(filepath.Dir(skillMD))
			if filepath.Dir(skillMD) == dir || !namespaceName.MatchString(bare) {
				continue
			}
			content, err := readText(skillMD)
			if err != nil {
				continue
			}
			fm, _ := Parse(content)
			p := Plugin{Namespace: source.Name, Name: bare, Path: skillMD, Description: fm.Description(), Frontmatter: fm}
			if !slices.ContainsFunc(out, func(o Plugin) bool { return o.Qualified() == p.Qualified() }) {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b Plugin) int { return strings.Compare(a.Qualified(), b.Qualified()) })
	return out
}

// Namespaced skill by its qualified name
func (s *Scope) plugin(qualified string) (Plugin, bool) {
	for _, p := range s.Plugins() {
		if p.Qualified() == qualified {
			return p, true
		}
	}
	return Plugin{}, false
}

// Names of the skills of one namespace, sorted
func (s *Scope) namespace(name string) []string {
	var out []string
	for _, p := range s.Plugins() {
		if p.Namespace == name {
			out = append(out, p.Name)
		}
	}
	return out
}

// Copies a folder of a tree to dest with each file's executable bit, folders made as needed; keep leaves files already there, and
// existing lets dest exist
func copyTree(files fs.FS, dir, dest string, existing, keep bool) error {
	if existing {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(dest, 0o755); err != nil {
			return err
		}
	}
	return fs.WalkDir(files, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if dir == "." {
			rel = p
		}
		if rel == "." || rel == "" {
			return nil
		}
		if runtimeCache(files, p, dir) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if keep && fsx.Lexists(target) {
			return nil
		}
		return copyFile(files, p, target)
	})
}

// Writes one file of a tree with its executable bit
func copyFile(files fs.FS, p, target string) error {
	data, err := fs.ReadFile(files, p)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := fs.Stat(files, p); err == nil && info.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// Folders of runtime state a tool leaves in a skill, never skill content
var runtimeDirs = map[string]bool{"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true}

// Whether a path of a tree is runtime state: under a runtime folder, or a compiled Python file beside its source
func runtimeCache(files fs.FS, p, skillDir string) bool {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, skillDir), "/")
	if skillDir == "." {
		rel = p
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		if runtimeDirs[part] {
			return true
		}
	}
	name := path.Base(p)
	if runtimeDirs[name] {
		if info, err := fs.Stat(files, p); err == nil && info.IsDir() {
			return true
		}
	}
	ext := path.Ext(name)
	if ext != ".pyc" && ext != ".pyo" {
		return false
	}
	info, err := fs.Stat(files, strings.TrimSuffix(p, ext)+".py")
	return err == nil && info.Mode().IsRegular()
}
