package skills

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How long a scanned catalog is trusted while its folders look unchanged, since editing a SKILL.md in place changes no folder
const catalogTTL = 30 * time.Second

// How long a scanned catalog is served without looking at its folders, for the many lookups one operation makes
const catalogBurst = 2 * time.Second

// How deep the change signature of a skill folder walks
const signatureDepth = 6

// How a name a lookup resolves stands
const (
	// The skill a lookup by its name loads
	Unique = "unique"
	// Hidden by a skill of the same name in a folder that wins, or by an identical copy beside it
	Shadowed = "shadowed"
	// Sharing its name with a different skill in a folder of the same precedence, loadable only by its path
	Ambiguous = "ambiguous"
)

// One skill found in a folder, with what a lookup by name resolves for it
type Entry struct {
	// Declared name, the folder's name when it has none
	Name        string
	Description string
	// First folder of a skill nested two or more folders below the profile's folder or an extra one, none otherwise
	Category string
	Tier     int
	// Skill folder it was found in
	Root string
	// Its SKILL.md
	Path string
	// Its folder relative to the root, with slashes
	Rel string
	// Whether its platforms, environments and apps let it be offered here
	Visible bool
	Status  string
	// Name a lookup loads it by, none when it can't be loaded by any
	Load        string
	Frontmatter Frontmatter
}

// Folder of the skill
func (e Entry) Dir() string {
	return filepath.Dir(e.Path)
}

type catalogCache struct {
	signature string
	at        time.Time
	entries   []Entry
}

// Drops every scanned catalog, so the next listing scans again
func (r *Registry) Forget() {
	st := r.shared()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.catalogs = nil
}

// Folder, its time and its child folders for a root and every folder below it a scan walks
func signature(root string, out *[]string, depth int) {
	info, err := os.Stat(root)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	skill := false
	for _, entry := range entries {
		if fsx.IsDir(filepath.Join(root, entry.Name())) {
			dirs = append(dirs, entry.Name())
		} else if entry.Name() == "SKILL.md" {
			skill = true
		}
	}
	slices.Sort(dirs)
	*out = append(*out, fmt.Sprintf("%s\x00%d\x00%s", root, info.ModTime().UnixNano(), strings.Join(dirs, "\x01")))
	if depth <= 0 {
		return
	}
	for _, name := range dirs {
		if !excluded[name] && !(skill && slices.Contains(supportDirs, name)) {
			signature(filepath.Join(root, name), out, depth-1)
		}
	}
}

// Change signature of the roots, the disabled names and the machine's platform
func (s *Scope) signature(roots []root, disabled map[string]bool) string {
	var parts []string
	for _, r := range roots {
		signature(r.dir, &parts, signatureDepth)
	}
	return strings.Join(parts, "\x02") + "\x03" + strings.Join(sorted(disabled), "\x01") + "\x03" + runtime.GOOS
}

// Sorted keys of a set
func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name, on := range set {
		if on {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Category of a skill from where it sits: the first folder when it is two or more folders below the profile's folder or an extra one
func (s *Scope) category(skillMD string) string {
	for _, dir := range append([]string{s.Dir()}, s.External()...) {
		rel, err := filepath.Rel(dir, skillMD)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= 3 {
			return parts[0]
		}
	}
	return ""
}

// Entry of one SKILL.md, false when its name or description can't be listed
func (s *Scope) scan(skillMD string, tier int, rootDir string) (Entry, bool) {
	content, err := readText(skillMD)
	if err != nil {
		slog.Debug("skills: could not read a skill", "path", skillMD, "err", err)
		return Entry{}, false
	}
	fm, body := Parse(content)
	raw := fm["description"]
	if !config.Truthy(raw) {
		raw = firstLine(body)
	}
	description := ""
	switch d := raw.(type) {
	case string:
		description = d
	case []any, map[string]any:
	default:
		slog.Debug("skills: skipping a skill whose description is not text", "path", skillMD)
		return Entry{}, false
	}
	name := filepath.Base(filepath.Dir(skillMD))
	if v, ok := fm["name"]; ok {
		declared, isText := v.(string)
		if !isText {
			slog.Debug("skills: skipping a skill whose name is not text", "path", skillMD)
			return Entry{}, false
		}
		name = declared
	}
	rel, err := filepath.Rel(rootDir, filepath.Dir(skillMD))
	if err != nil {
		rel = filepath.Dir(skillMD)
	}
	return Entry{
		Name:        strs.Head(name, MaxName),
		Description: capDescription(description),
		Category:    s.category(skillMD),
		Tier:        tier,
		Root:        rootDir,
		Path:        skillMD,
		Rel:         filepath.ToSlash(rel),
		Visible:     s.compatible(fm),
		Frontmatter: fm,
	}, true
}

// First line of a body that is not a heading
func firstLine(body string) string {
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// Order of copies of one skill in one folder: a SKILL.md before a flat markdown file, shallower first
func rank(skillMD, rootDir string) [2]int {
	flat := 0
	if filepath.Base(skillMD) != "SKILL.md" {
		flat = 1
	}
	depth := 0
	if rel, err := filepath.Rel(rootDir, skillMD); err == nil {
		depth = len(strings.Split(filepath.ToSlash(rel), "/"))
	}
	return [2]int{flat, depth}
}

// One place a name can resolve to
type candidate struct {
	tier int
	root string
	rank [2]int
	path string
}

// Candidate a name resolves to and the contenders of the winning tier, the winner -1 when they differ and none can be picked
func pick(candidates []candidate) (int, []int) {
	top := slices.MinFunc(candidates, func(a, b candidate) int { return cmp.Compare(a.tier, b.tier) }).tier
	var contenders []int
	for i, c := range candidates {
		if c.tier == top {
			contenders = append(contenders, i)
		}
	}
	if len(contenders) == 1 {
		return contenders[0], contenders
	}
	sameRoot := true
	var paths []string
	for _, i := range contenders {
		sameRoot = sameRoot && candidates[i].root == candidates[contenders[0]].root
		paths = append(paths, candidates[i].path)
	}
	if sameRoot && same(paths) {
		ranked := slices.Clone(contenders)
		slices.SortStableFunc(ranked, func(a, b int) int {
			ra, rb := candidates[a].rank, candidates[b].rank
			return cmp.Or(cmp.Compare(ra[0], rb[0]), cmp.Compare(ra[1], rb[1]))
		})
		if candidates[ranked[0]].rank != candidates[ranked[1]].rank {
			return ranked[0], contenders
		}
	}
	return -1, contenders
}

// Gives every entry the status and load name a lookup resolves for it: by its name, its folder's name and its path
func (s *Scope) resolveEntries(entries []Entry) []Entry {
	out := slices.Clone(entries)
	owners := map[string][]int{}
	for i, e := range out {
		for _, alias := range uniqueStrings(e.Name, filepath.Base(e.Dir()), e.Rel) {
			owners[alias] = append(owners[alias], i)
		}
	}
	winner := map[string]int{}
	for alias, indexes := range owners {
		candidates := make([]candidate, len(indexes))
		for k, j := range indexes {
			candidates[k] = candidate{out[j].Tier, out[j].Root, rank(out[j].Path, out[j].Root), out[j].Path}
		}
		if won, _ := pick(candidates); won < 0 {
			winner[alias] = -1
		} else {
			winner[alias] = indexes[won]
		}
	}
	for i := range out {
		name := out[i].Name
		var higher []string
		for _, j := range owners[name] {
			if out[j].Tier < out[i].Tier {
				higher = append(higher, out[j].Path)
			}
		}
		switch {
		case len(higher) > 0 || winner[name] != -1 && winner[name] != i:
			out[i].Status, out[i].Load = Shadowed, ""
			if len(higher) > 0 && s.registry.first("shadow\x00"+out[i].Path+"\x00"+strings.Join(sortedList(higher), "\x00")) &&
				!slices.ContainsFunc(higher, func(p string) bool { return same([]string{out[i].Path, p}) }) {
				slog.Warn("skills: a skill is shadowed by a copy in a folder that wins (project > profile > create_dir > external_dirs)", "skill", name, "path", out[i].Path)
			}
		case winner[name] == i:
			out[i].Status, out[i].Load = Unique, name
		default:
			out[i].Status, out[i].Load = Ambiguous, ""
			if winner[out[i].Rel] == i {
				out[i].Load = out[i].Rel
			}
		}
	}
	return out
}

// Strings without repeats, in order
func uniqueStrings(values ...string) []string {
	var out []string
	for _, v := range values {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Every skill of every folder resolved, the visible ones unless hidden ones are asked for, without the disabled unless asked for
func (s *Scope) catalog(withDisabled, withHidden bool) []Entry {
	disabled := map[string]bool{}
	if !withDisabled {
		disabled = s.Disabled()
	}
	roots := s.searched()
	var dirs []string
	for _, r := range roots {
		dirs = append(dirs, r.dir)
	}
	key := fmt.Sprint(withDisabled, withHidden, s.Toolsets["kanban"], s.Platform, dirs)
	now := time.Now()
	st := s.registry.shared()
	st.mu.Lock()
	cached, ok := st.catalogs[key]
	st.mu.Unlock()
	if ok && now.Sub(cached.at) < catalogBurst {
		return slices.Clone(cached.entries)
	}
	sig := s.signature(roots, disabled)
	if ok && cached.signature == sig && now.Sub(cached.at) < catalogTTL {
		return slices.Clone(cached.entries)
	}
	var scanned []Entry
	for _, rt := range roots {
		files := skillFiles(rt.dir)
		if rt.tier == TierProject {
			files = s.unquarantined(files)
		}
		for _, skillMD := range files {
			if entry, ok := s.scan(skillMD, rt.tier, rt.dir); ok {
				scanned = append(scanned, entry)
			}
		}
	}
	var entries []Entry
	for _, e := range s.resolveEntries(scanned) {
		if (e.Visible || withHidden) && !disabled[e.Name] && (e.Load == "" || !disabled[e.Load]) {
			entries = append(entries, e)
		}
	}
	st.mu.Lock()
	if st.catalogs == nil {
		st.catalogs = map[string]catalogCache{}
	}
	st.catalogs[key] = catalogCache{signature: sig, at: now, entries: entries}
	st.mu.Unlock()
	return slices.Clone(entries)
}

// Skills a lookup can load by name, the disabled ones too when asked, ordered by category then name
func (s *Scope) Loadable(withDisabled bool) []Entry {
	var out []Entry
	for _, e := range s.catalog(withDisabled, false) {
		if e.Load != "" {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return cmp.Or(cmp.Compare(a.Category, b.Category), cmp.Compare(a.Load, b.Load)) })
	return out
}

// Whether the catalog with hidden and disabled skills loads this file by the relative name
func (s *Scope) loadsAs(skillMD, rel string) bool {
	return slices.ContainsFunc(s.catalog(true, true), func(e Entry) bool { return e.Path == skillMD && e.Load == rel })
}
