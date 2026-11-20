package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Progressive disclosure limits on a listed skill name and description
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
)

// Catalog cache lifetime and the directory depth its change signature walks
const (
	skillsCacheTTL     = 30 * time.Second
	signatureMaxDepth  = 6
	catalogScanPrefix  = 4000
	categoryPartsFloor = 3
)

// One cached catalog with the change signature and scan time it was built under
type catalogCache struct {
	signature string
	at        time.Time
	skills    []catalogEntry
}

var (
	skillsCacheMu sync.Mutex
	skillsCache   = map[string]catalogCache{}
)

// Drops every scanned catalog so the next listing scans again without waiting out the TTL
func ClearSkillsCache() {
	skillsCacheMu.Lock()
	skillsCache = map[string]catalogCache{}
	skillsCacheMu.Unlock()
}

// Appends dir, mtime and child dir names for root and every catalog walked dir below it
func dirTreeSignature(root string, sig *[]string, depth int) {
	st, err := os.Stat(root)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var names []string
	isSkill := false
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			names = append(names, e.Name())
		} else if e.Name() == "SKILL.md" {
			isSkill = true
		}
	}
	sort.Strings(names)
	*sig = append(*sig, fmt.Sprintf("%s\x00%d\x00%s", root, st.ModTime().UnixNano(), strings.Join(names, "\x01")))
	if depth <= 0 {
		return
	}
	for _, name := range names {
		if !ExcludedSkillDirs[name] && !(isSkill && SkillSupportDirs[name]) {
			dirTreeSignature(filepath.Join(root, name), sig, depth-1)
		}
	}
}

// Change signature over every catalog walked dir, the disabled set and the host platform
func skillsScanSignature(dirs []string, disabled map[string]bool) string {
	var sig []string
	for _, d := range dirs {
		dirTreeSignature(d, &sig, signatureMaxDepth)
	}
	names := make([]string, 0, len(disabled))
	for n := range disabled {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(sig, "\x02") + "\x03" + strings.Join(names, "\x01") + "\x03" + runtime.GOOS
}

// Skill roots in precedence order with an absent local dir dropped, plus the active skills dir
func (s *Scope) skillSearchDirs() ([]skillRoot, string) {
	active := s.SkillsDir()
	var roots []skillRoot
	for _, r := range s.skillSearchRoots(active, true) {
		if r.tier != TierLocal || lexistsFollow(r.dir) {
			roots = append(roots, r)
		}
	}
	return roots, active
}

// Reports whether a path exists following symlinks
func lexistsFollow(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Category of a skill three or more parts below the active or an external skills dir
func (s *Scope) categoryFromPath(skillPath string) string {
	dirs := append([]string{s.SkillsDir()}, s.ExternalSkillsDirs()...)
	for _, d := range dirs {
		if !isRelativeTo(skillPath, d) {
			continue
		}
		rel, err := filepath.Rel(d, skillPath)
		if err != nil || rel == "." {
			continue
		}
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= categoryPartsFloor {
			return parts[0]
		}
	}
	return ""
}

// Tags from frontmatter as a parsed list, a bracketed string or a comma list
func parseTags(v any) []string {
	out := []string{}
	if !fmTruthy(v) {
		return out
	}
	if list, ok := v.([]any); ok {
		for _, t := range list {
			if fmTruthy(t) {
				out = append(out, strings.TrimSpace(fmString(t)))
			}
		}
		return out
	}
	text := strings.TrimSpace(fmString(v))
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = text[1 : len(text)-1]
	}
	for _, t := range strings.Split(text, ",") {
		if strings.TrimSpace(t) != "" {
			out = append(out, strings.Trim(strings.TrimSpace(t), "\"'"))
		}
	}
	return out
}

// Description capped at the disclosure limit with an ellipsis
func truncateDescription(d string) string {
	if utf8.RuneCountInString(d) <= MaxDescriptionLength {
		return d
	}
	return string([]rune(d)[:MaxDescriptionLength-3]) + "..."
}

// First characters of text up to n runes
func runeHead(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}

// Listed entry of one SKILL.md, false when its name or description cannot be listed
func (s *Scope) scanCatalogEntry(skillMD string, tier int, root string) (catalogEntry, bool) {
	text, err := readSkillText(skillMD)
	if err != nil {
		s.logger().Debug(fmt.Sprintf("Failed to read skill file %s: %v", skillMD, err))
		return catalogEntry{}, false
	}
	fm, body := ParseFrontmatter(runeHead(text, catalogScanPrefix))
	descRaw, hasDesc := fm["description"]
	if !hasDesc {
		descRaw = ""
	}
	if !fmTruthy(descRaw) {
		for _, ln := range strings.Split(strings.TrimSpace(body), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
				descRaw = ln
				break
			}
		}
	}
	var desc string
	switch d := descRaw.(type) {
	case string:
		desc = d
	case []any, map[string]any:
		desc = fmString(d)
	default:
		s.logger().Debug(fmt.Sprintf("Skipping skill at %s: failed to parse: description has no length", skillMD))
		return catalogEntry{}, false
	}
	name := filepath.Base(filepath.Dir(skillMD))
	if v, ok := fm["name"]; ok {
		n, isStr := v.(string)
		if !isStr {
			s.logger().Debug(fmt.Sprintf("Skipping skill at %s: failed to parse: name is not a string", skillMD))
			return catalogEntry{}, false
		}
		name = n
	}
	return catalogEntry{
		Name: runeHead(name, MaxNameLength), Description: truncateDescription(desc),
		Category: s.categoryFromPath(skillMD), Tier: tier, Root: root, Path: skillMD,
		Visible: skillMatchesPlatform(fm) && s.skillMatchesEnvironment(fm) && skillMatchesApps(fm),
	}, true
}

// Every scanned skill resolved for status and load name, visible ones unless includeHidden, cached per signature
func (s *Scope) skillCatalog(skipDisabled, includeHidden bool) []catalogEntry {
	cacheKey := fmt.Sprintf("%t/%t", skipDisabled, includeHidden)
	disabled := map[string]bool{}
	if !skipDisabled {
		disabled = s.DisabledSkillNames("")
	}
	roots, _ := s.skillSearchDirs()
	dirs := make([]string, len(roots))
	for i, r := range roots {
		dirs[i] = r.dir
	}
	signature := skillsScanSignature(dirs, disabled)
	now := time.Now()
	skillsCacheMu.Lock()
	cached, ok := skillsCache[cacheKey]
	skillsCacheMu.Unlock()
	if ok && cached.signature == signature && now.Sub(cached.at) < skillsCacheTTL {
		return append([]catalogEntry(nil), cached.skills...)
	}
	var scanned []catalogEntry
	for _, r := range roots {
		var files []string
		if r.tier == TierProject {
			files = s.iterProjectSkillFiles(r.dir)
		} else {
			files = iterSkillIndexFiles(r.dir, "SKILL.md")
		}
		for _, skillMD := range files {
			if hasExcludedPart(skillMD) {
				continue
			}
			if e, ok := s.scanCatalogEntry(skillMD, r.tier, r.dir); ok {
				scanned = append(scanned, e)
			}
		}
	}
	var skills []catalogEntry
	for _, e := range s.resolveSkillCatalog(scanned) {
		if (e.Visible || includeHidden) && !isDisabledEntry(e, disabled) {
			skills = append(skills, e)
		}
	}
	skillsCacheMu.Lock()
	skillsCache[cacheKey] = catalogCache{signature: signature, at: now, skills: skills}
	skillsCacheMu.Unlock()
	return append([]catalogEntry(nil), skills...)
}

// Reports whether any component of a path is an excluded directory name
func hasExcludedPart(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if ExcludedSkillDirs[part] {
			return true
		}
	}
	return false
}

// Loadable skills as name, description and category, the name being what skill_view accepts
func (s *Scope) FindAllSkills(skipDisabled bool) []map[string]any {
	out := []map[string]any{}
	for _, e := range s.skillCatalog(skipDisabled, false) {
		if e.LoadName != "" {
			out = append(out, map[string]any{"name": e.LoadName, "description": e.Description, "category": nullableCategory(e.Category)})
		}
	}
	return out
}

// Category as a JSON value, null for an uncategorized skill
func nullableCategory(c string) any {
	if c == "" {
		return nil
	}
	return c
}

// Orders listings by category then name
func SortSkills(skills []map[string]any) []map[string]any {
	out := append([]map[string]any(nil), skills...)
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := fmString(out[i]["category"]), fmString(out[j]["category"])
		if ci != cj {
			return ci < cj
		}
		return fmString(out[i]["name"]) < fmString(out[j]["name"])
	})
	return out
}
