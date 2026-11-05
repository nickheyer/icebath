package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	skillsPromptCacheMax   = 32
	skillsSnapshotVersion  = 5
	skillsSnapshotFilename = ".skills_prompt_snapshot.json"
)

const oneshotSkillsLoadGuidance = "## Skills\n" +
	"Scan the skills below and load one with skill_view(name) only when it carries domain knowledge you lack " +
	"for THIS task (an API, a tool's commands, a project's conventions). Do not load general process skills " +
	"(testing, debugging, review methodology) for work you already know how to do.\n"

// Gateway session policy source governing the turn, set by the gateway host, false when no policy is active
var ActivePolicySource func(env *tools.Env) (source string, ok bool)

var skillsPromptCache = struct {
	mu    sync.Mutex
	order []string
	vals  map[string]string
}{vals: map[string]string{}}

// Options of one skills index build, nil tool sets meaning no filtering information
type SkillsPromptOptions struct {
	AvailableTools    map[string]bool
	AvailableToolsets map[string]bool
	CompactCategories map[string]bool
	SkillsDirOverride string
}

// One index row as name and description
type indexRow struct{ name, desc string }

// Drops the in process skills prompt cache and with disk also the home snapshot file
func ClearSkillsSystemPromptCache(home string, disk bool) {
	skillsPromptCache.mu.Lock()
	skillsPromptCache.order = nil
	skillsPromptCache.vals = map[string]string{}
	skillsPromptCache.mu.Unlock()
	if !disk {
		return
	}
	p := filepath.Join(home, skillsSnapshotFilename)
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		slog.Debug(fmt.Sprintf("Could not remove skills prompt snapshot: [Errno 21] Is a directory: %s", pyrepr.StrRepr(p)))
		return
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug(fmt.Sprintf("Could not remove skills prompt snapshot: %v", err))
	}
}

// File signature manifest of every SKILL.md and DESCRIPTION.md keyed by path relative to the skills dir
func buildSkillsManifest(skillsDir string) *pyjson.Object {
	manifest := pyjson.NewObject()
	prefix := filepath.Clean(skillsDir)
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	walkFollowingLinks(skillsDir, pruneIndexDirs, func(dir string, files []string) {
		for _, name := range []string{"SKILL.md", "DESCRIPTION.md"} {
			if !slices.Contains(files, name) {
				continue
			}
			p := filepath.Join(dir, name)
			if sig, err := config.FileCacheVersion(p, true); err == nil {
				manifest.Set(strings.TrimPrefix(p, prefix), sig)
			}
		}
	})
	return manifest
}

// Children of a walked dir minus excluded trees and the support dirs of a skill
func pruneIndexDirs(_ string, names []string, hasSkillMD bool) []string {
	kept := names[:0]
	for _, d := range names {
		if !ExcludedSkillDirs[d] && !(hasSkillMD && SkillSupportDirs[d]) {
			kept = append(kept, d)
		}
	}
	return kept
}

// Disk snapshot when it parses, carries the current version, and its manifest matches
func loadSkillsSnapshot(home string, manifest *pyjson.Object) *pyjson.Object {
	text, err := readSkillMDStrict(filepath.Join(home, skillsSnapshotFilename))
	if err != nil {
		return nil
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	snap, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil
	}
	version, _ := snap.Get("version")
	stored, _ := snap.Get("manifest")
	if !pyrepr.Equal(version, int64(skillsSnapshotVersion)) || !pyrepr.Equal(stored, manifest) {
		return nil
	}
	return snap
}

// Items Python iteration of a frontmatter value or [] yields, a TypeError for scalars
func iterOrEmpty(v any) ([]any, error) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		keys := make([]any, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].(string) < keys[j].(string) })
		return keys, nil
	}
	if _, ok := v.(map[string]any); ok {
		return []any{}, nil
	}
	return ledgerIter(v)
}

// Stripped non empty str forms of items
func strippedStrs(items []any) []any {
	out := []any{}
	for _, it := range items {
		if s := pystr.Strip(pyrepr.Str(it)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// requires_apps of a frontmatter as stripped names, a scalar becoming a one item list
func requiresAppsList(fm map[string]any) []any {
	raw := fm["requires_apps"]
	var items []any
	switch t := raw.(type) {
	case []any:
		items = t
	default:
		if pyrepr.Truthy(raw) {
			items = []any{raw}
		}
	}
	return strippedStrs(items)
}

// Serialisable index metadata of one skill file relative to its root
func buildSnapshotEntry(skillFile, skillsDir string, fm map[string]any, description string) (*pyjson.Object, error) {
	rel, err := filepath.Rel(skillsDir, skillFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is not in the subpath of %s", pyrepr.StrRepr(skillFile), pyrepr.StrRepr(skillsDir))
	}
	parts := strings.Split(rel, string(filepath.Separator))
	skillName := filepath.Base(filepath.Dir(skillFile))
	category := "general"
	if len(parts) > 2 {
		category = strings.Join(parts[:len(parts)-2], "/")
	} else if len(parts) == 2 {
		category = parts[0]
	}
	fmName := skillName
	if v, ok := fm["name"]; ok {
		fmName = pyrepr.Str(v)
	}
	platformsRaw := fm["platforms"]
	if s, ok := platformsRaw.(string); ok && s != "" {
		platformsRaw = []any{s}
	}
	platforms, err := iterOrEmpty(platformsRaw)
	if err != nil {
		return nil, err
	}
	conditions := pyjson.NewObject()
	for k, v := range extractSkillConditions(fm) {
		conditions.Set(k, toAnyList(v))
	}
	sort.SliceStable(conditions.Keys, func(i, j int) bool {
		return slices.Index(conditionKeys, conditions.Keys[i]) < slices.Index(conditionKeys, conditions.Keys[j])
	})
	entry := pyjson.NewObject()
	entry.Set("skill_name", skillName)
	entry.Set("category", category)
	entry.Set("frontmatter_name", fmName)
	entry.Set("rel", filepath.ToSlash(rel))
	entry.Set("description", description)
	entry.Set("platforms", strippedStrs(platforms))
	entry.Set("conditions", conditions)
	entry.Set("requires_apps", requiresAppsList(fm))
	return entry, nil
}

// Strings as a JSON list value
func toAnyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Reads a SKILL.md once into its offer time compatibility, frontmatter and description
func (s *Scope) parseSkillFile(skillFile string) (bool, map[string]any, string) {
	raw, err := readSkillMDStrict(skillFile)
	if err != nil {
		s.logger().Warn(fmt.Sprintf("Failed to parse skill file %s: %v", skillFile, err))
		return true, map[string]any{}, ""
	}
	fm, _ := ParseFrontmatter(raw)
	compatible := skillMatchesPlatform(fm) && s.skillMatchesEnvironment(fm) && skillMatchesApps(fm)
	return compatible, fm, extractSkillDescription(fm)
}

// False when the conditional activation rules or the session platform gate exclude the skill
func skillShouldShow(conditions map[string][]string, availableTools, availableToolsets map[string]bool, sessionPlatform string) bool {
	var wanted []string
	for _, p := range conditions["session_platforms"] {
		if v := pystr.Lower(pystr.Strip(p)); pystr.Strip(p) != "" {
			wanted = append(wanted, v)
		}
	}
	if len(wanted) > 0 && sessionPlatform != "" && !slices.Contains(wanted, pystr.Lower(pystr.Strip(sessionPlatform))) {
		return false
	}
	if availableTools == nil && availableToolsets == nil {
		return true
	}
	for _, ts := range conditions["fallback_for_toolsets"] {
		if availableToolsets[ts] {
			return false
		}
	}
	for _, t := range conditions["fallback_for_tools"] {
		if availableTools[t] {
			return false
		}
	}
	for _, ts := range conditions["requires_toolsets"] {
		if !availableToolsets[ts] {
			return false
		}
	}
	for _, t := range conditions["requires_tools"] {
		if !availableTools[t] {
			return false
		}
	}
	return true
}

// Qualified name and description of every enabled plugin skill passing the offer time gates
func (s *Scope) pluginSkillPromptRows(ctx context.Context, disabled, availableTools, availableToolsets map[string]bool, sessionPlatform string) []indexRow {
	host, err := s.pluginHost(ctx)
	if err != nil {
		s.logger().Debug("Plugin skill prompt rows unavailable", "error", err)
		return nil
	}
	host.Discover(ctx)
	var rows []indexRow
	for _, meta := range host.ListPluginSkillMetadata() {
		name := pyStrTruthy(meta["name"])
		if name == "" || disabled[name] {
			continue
		}
		fm := map[string]any{}
		if raw := meta["frontmatter"]; pyrepr.Truthy(raw) {
			m, ok := raw.(map[string]any)
			if !ok {
				s.logger().Debug("Plugin skill prompt rows unavailable", "error", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw)))
				return rows
			}
			fm = m
		}
		if !(skillMatchesPlatform(fm) && s.skillMatchesEnvironment(fm) && skillMatchesApps(fm)) {
			continue
		}
		if !skillShouldShow(extractSkillConditions(fm), availableTools, availableToolsets, sessionPlatform) {
			continue
		}
		desc := ClipSkillDescription(pystr.Strip(pyStrTruthy(meta["description"])))
		if desc == "" {
			desc = extractSkillDescription(fm)
		}
		rows = append(rows, indexRow{name, desc})
	}
	return rows
}

// Active session platform from the process env, then the bound session context
func (s *Scope) currentSessionPlatformHint() string {
	if p := os.Getenv("ICEBATH_PLATFORM"); p != "" {
		return p
	}
	if p := os.Getenv("ICEBATH_SESSION_PLATFORM"); p != "" {
		return p
	}
	return tools.SessionEnvValue(s.Env, "ICEBATH_SESSION_PLATFORM")
}

// Reports a finite single query session from the gateway policy, then the session env marker
func (s *Scope) singleQuerySession() bool {
	if ActivePolicySource != nil {
		if source, ok := ActivePolicySource(s.Env); ok {
			return source == "oneshot"
		}
	}
	return tools.SessionEnvValue(s.Env, "ICEBATH_SINGLE_QUERY_SESSION") == "1"
}

// Compact skill index for the system prompt, empty under a safe worker or with no skill roots
func (s *Scope) BuildSkillsSystemPrompt(ctx context.Context, opts SkillsPromptOptions) (string, error) {
	if config.SafeWorkerEnabled() {
		return "", nil
	}
	sc, skillsDir := s, s.SkillsDir()
	if opts.SkillsDirOverride != "" {
		skillsDir = opts.SkillsDirOverride
		sc = s.scopeFor(ctx, filepath.Dir(skillsDir), s.Cwd)
		sc.Cwd = s.Cwd
		sc.SkillsDirOverride = skillsDir
	}
	var extra []skillRoot
	for _, r := range sc.skillSearchRoots(skillsDir, true) {
		if r.tier != TierLocal {
			extra = append(extra, r)
		}
	}
	if !lexistsFollow(skillsDir) && len(extra) == 0 {
		return "", nil
	}
	return sc.buildSkillsSystemPromptInner(ctx, skillsDir, extra, opts.AvailableTools, opts.AvailableToolsets, opts.CompactCategories)
}

// Declared name of an index entry, the dir name when the frontmatter has none
func entryName(e *pyjson.Object) string {
	if v, _ := e.Get("frontmatter_name"); pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	v, _ := e.Get("skill_name")
	return pyStrTruthy(v)
}

// One scanned index entry with its root, tier and offer time compatibility
type indexCandidate struct {
	entry      *pyjson.Object
	tier       int
	root       string
	compatible bool
}

// Index entries of every skill file under a project, create_dir or external root
func (s *Scope) scanExtraRoot(root string, files []string, tier int, what string) []indexCandidate {
	var rows []indexCandidate
	for _, f := range files {
		ok, fm, desc := s.parseSkillFile(f)
		entry, err := buildSnapshotEntry(f, root, fm, desc)
		if err != nil {
			s.logger().Debug(fmt.Sprintf("Error reading %s skill %s: %v", what, f, err))
			continue
		}
		rows = append(rows, indexCandidate{entry, tier, root, ok})
	}
	return rows
}

// Skills listed by snapshot entries when the value iterates, a TypeError for scalars and null
func snapshotSkills(snap *pyjson.Object) ([]*pyjson.Object, error) {
	raw, ok := snap.Get("skills")
	if !ok {
		return nil, nil
	}
	var items []any
	switch t := raw.(type) {
	case []any:
		items = t
	case *pyjson.Object, string:
	default:
		return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
	}
	var out []*pyjson.Object
	for _, it := range items {
		if o, ok := it.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out, nil
}

// Value of an entry key, nil when absent
func entryGet(e *pyjson.Object, key string) any {
	v, _ := e.Get(key)
	return v
}

// Conditions of an entry as string lists keyed by condition name
func entryConditions(e *pyjson.Object) map[string][]string {
	out := map[string][]string{}
	if o, ok := entryGet(e, "conditions").(*pyjson.Object); ok {
		for _, k := range o.Keys {
			out[k] = fmStringList(jsonToFrontmatter(o.Vals[k]))
		}
	}
	return out
}

// Converts decoded JSON values into the frontmatter value model
func jsonToFrontmatter(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		m := map[string]any{}
		for _, k := range t.Keys {
			m[k] = jsonToFrontmatter(t.Vals[k])
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonToFrontmatter(x)
		}
		return out
	}
	return v
}

// Cache key text of every input the rendered index depends on
func skillsPromptCacheKey(parts ...any) string {
	text, err := pyjson.Dumps(parts, pyjson.SortKeys(true))
	if err != nil {
		return fmt.Sprint(parts...)
	}
	return text
}

// Index over every tier resolved like skill_view, served from the LRU unless an app gate forces a rebuild
func (s *Scope) buildSkillsSystemPromptInner(ctx context.Context, skillsDir string, extra []skillRoot, availableTools, availableToolsets, compact map[string]bool) (string, error) {
	platformHint := s.currentSessionPlatformHint()
	disabled := s.DisabledSkillNames(platformHint)
	pluginRows := s.pluginSkillPromptRows(ctx, disabled, availableTools, availableToolsets, platformHint)
	manifest := buildSkillsManifest(skillsDir)
	var files []string
	addFiles := func(prefix []any, m *pyjson.Object) {
		for _, rel := range m.Keys {
			row := append(append([]any{}, prefix...), rel)
			row = append(row, m.Vals[rel].([]any)...)
			files = append(files, skillsPromptCacheKey(row...))
		}
	}
	addFiles(nil, manifest)
	var extraKey []any
	for _, r := range extra {
		addFiles([]any{r.dir}, buildSkillsManifest(r.dir))
		extraKey = append(extraKey, []any{r.tier, r.dir})
	}
	sort.Strings(files)
	files = slices.Compact(files)
	var pluginKey []any
	for _, r := range pluginRows {
		pluginKey = append(pluginKey, []any{r.name, r.desc})
	}
	key := skillsPromptCacheKey(skillsDir, extraKey, files, sortedSet(availableTools), sortedSet(availableToolsets),
		platformHint, sortedSet(disabled), sortedSet(compact), s.singleQuerySession(), pluginKey)
	snapshot := loadSkillsSnapshot(s.Home, manifest)
	var snapEntries []*pyjson.Object
	appGated := false
	if snapshot != nil {
		var err error
		if snapEntries, err = snapshotSkills(snapshot); err != nil {
			return "", err
		}
		for _, e := range snapEntries {
			if pyrepr.Truthy(entryGet(e, "requires_apps")) {
				appGated = true
			}
		}
	}
	skillsPromptCache.mu.Lock()
	if cached, ok := skillsPromptCache.vals[key]; ok && !appGated {
		touchPromptCache(key)
		skillsPromptCache.mu.Unlock()
		return cached, nil
	}
	skillsPromptCache.mu.Unlock()

	hides := func(skillName string, conditions map[string][]string) bool {
		return disabled[skillName] || !skillShouldShow(conditions, availableTools, availableToolsets, platformHint)
	}
	var candidates []indexCandidate
	if snapshot != nil {
		for _, e := range snapEntries {
			platforms := jsonToFrontmatter(entryGet(e, "platforms"))
			if !pyrepr.Truthy(platforms) {
				platforms = []any{}
			}
			apps := jsonToFrontmatter(entryGet(e, "requires_apps"))
			if !pyrepr.Truthy(apps) {
				apps = []any{}
			}
			ok := skillMatchesPlatformList(platforms) && skillMatchesApps(map[string]any{"requires_apps": apps})
			candidates = append(candidates, indexCandidate{e, TierLocal, skillsDir, ok})
		}
	} else {
		for _, f := range iterSkillIndexFiles(skillsDir, "SKILL.md") {
			ok, fm, desc := s.parseSkillFile(f)
			entry, err := buildSnapshotEntry(f, skillsDir, fm, desc)
			if err != nil {
				return "", err
			}
			candidates = append(candidates, indexCandidate{entry, TierLocal, skillsDir, ok})
		}
	}
	var rows []indexCandidate
	for _, r := range extra {
		if r.tier == TierProject && lexistsFollow(r.dir) {
			rows = append(rows, s.scanExtraRoot(r.dir, s.iterProjectSkillFiles(r.dir), TierProject, "project")...)
		}
	}
	rows = append(rows, candidates...)
	for _, r := range extra {
		if r.tier != TierProject && lexistsFollow(r.dir) {
			rows = append(rows, s.scanExtraRoot(r.dir, iterSkillIndexFiles(r.dir, "SKILL.md"), r.tier, "external")...)
		}
	}
	entries := make([]catalogEntry, 0, len(rows))
	for _, r := range rows {
		rel, ok := entryGet(r.entry, "rel").(string)
		if !ok {
			if _, present := r.entry.Get("rel"); !present {
				return "", fmt.Errorf("KeyError: 'rel'")
			}
			return "", fmt.Errorf("unsupported operand type(s) for /: 'PosixPath' and '%s'", pyrepr.TypeName(entryGet(r.entry, "rel")))
		}
		conds := entryConditions(r.entry)
		category := pyStrTruthy(entryGet(r.entry, "category"))
		if category == "" {
			category = "general"
		}
		desc := entryGet(r.entry, "description")
		entries = append(entries, catalogEntry{
			Name: entryName(r.entry), Description: pyStrTruthy(desc), Category: category, Tier: r.tier, Root: r.root,
			Path: filepath.Join(r.root, filepath.FromSlash(rel)), Visible: r.compatible && !hides(pyStrTruthy(entryGet(r.entry, "skill_name")), conds),
			SkillName: pyStrTruthy(entryGet(r.entry, "skill_name")), Conditions: conds, Rel: rel,
		})
	}
	resolved := s.resolveSkillCatalog(entries)
	byCategory := map[string][]indexRow{}
	unloadableSet := map[string]bool{}
	for _, e := range resolved {
		if !e.Visible || e.Status == "shadowed" || isDisabledEntry(e, disabled) {
			continue
		}
		if e.LoadName == "" {
			unloadableSet[e.Name] = true
			continue
		}
		desc := e.Description
		if e.Tier == TierProject {
			desc = pystr.Strip("[project] " + desc)
		}
		byCategory[e.Category] = append(byCategory[e.Category], indexRow{e.LoadName, desc})
	}
	if len(pluginRows) > 0 {
		listed := map[string]bool{}
		for _, rs := range byCategory {
			for _, r := range rs {
				listed[r.name] = true
			}
		}
		for _, r := range pluginRows {
			if !listed[r.name] {
				listed[r.name] = true
				byCategory["plugin"] = append(byCategory["plugin"], r)
			}
		}
	}
	if snapshot == nil {
		snap := pyjson.NewObject()
		snap.Set("version", int64(skillsSnapshotVersion))
		snap.Set("manifest", buildSkillsManifest(skillsDir))
		list := make([]any, len(candidates))
		for i, c := range candidates {
			list[i] = c.entry
		}
		snap.Set("skills", list)
		if err := config.AtomicJSONWrite(filepath.Join(s.Home, skillsSnapshotFilename), snap); err != nil {
			s.logger().Debug(fmt.Sprintf("Could not write skills prompt snapshot: %v", err))
		}
	}
	var unloadable []string
	for n := range unloadableSet {
		unloadable = append(unloadable, n)
	}
	sort.Strings(unloadable)
	result := s.renderSkillsIndex(byCategory, compact, availableTools, unloadable)
	skillsPromptCache.mu.Lock()
	skillsPromptCache.vals[key] = result
	touchPromptCache(key)
	for len(skillsPromptCache.order) > skillsPromptCacheMax {
		delete(skillsPromptCache.vals, skillsPromptCache.order[0])
		skillsPromptCache.order = skillsPromptCache.order[1:]
	}
	skillsPromptCache.mu.Unlock()
	return result, nil
}

// Moves a cache key to the most recent end, caller holds the lock
func touchPromptCache(key string) {
	if i := slices.Index(skillsPromptCache.order, key); i >= 0 {
		skillsPromptCache.order = slices.Delete(skillsPromptCache.order, i, i+1)
	}
	skillsPromptCache.order = append(skillsPromptCache.order, key)
}

// Renders the skills block with demoted categories as names only lines, empty when nothing is listed
func (s *Scope) renderSkillsIndex(byCategory map[string][]indexRow, compact, availableTools map[string]bool, unloadable []string) string {
	if len(byCategory) == 0 {
		return ""
	}
	demoted := map[string]bool{}
	for cat := range byCategory {
		if compact[strings.SplitN(cat, "/", 2)[0]] {
			demoted[cat] = true
		}
	}
	hiddenNote := ""
	if len(demoted) > 0 {
		hiddenNote = "\n(Categories marked [names only] are outside the current coding " +
			"context, so their descriptions are omitted — the skills work " +
			"normally and load with skill_view(name) as usual.)"
	}
	if len(unloadable) > 0 {
		hiddenNote += "\n(A copy of " + strings.Join(unloadable, ", ") + " is not listed: it shares both its name and its path " +
			"with a different skill in the same skills directory tier (e.g. another external_dirs entry), " +
			"so skill_view cannot load it — rename one.)"
	}
	basicTools := "web_search or terminal"
	if availableTools != nil && !availableTools["web_search"] {
		basicTools = "terminal"
	}
	cats := make([]string, 0, len(byCategory))
	for c := range byCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var lines []string
	for _, cat := range cats {
		entries := byCategory[cat]
		if demoted[cat] {
			names := map[string]bool{}
			for _, e := range entries {
				names[e.name] = true
			}
			list := make([]string, 0, len(names))
			for n := range names {
				list = append(list, n)
			}
			sort.Strings(list)
			lines = append(lines, "  "+cat+" [names only]: "+strings.Join(list, ", "))
			continue
		}
		sorted := slices.Clone(entries)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
		var rows []indexRow
		seen := map[string]bool{}
		for _, e := range sorted {
			if !seen[e.name] {
				seen[e.name] = true
				rows = append(rows, e)
			}
		}
		indent := "    "
		if len(rows) == 1 {
			indent = "  "
		} else {
			lines = append(lines, "  "+cat+":")
		}
		for _, r := range rows {
			if r.desc != "" {
				lines = append(lines, indent+"- "+r.name+": "+r.desc)
			} else {
				lines = append(lines, indent+"- "+r.name)
			}
		}
	}
	if s.singleQuerySession() {
		return oneshotSkillsLoadGuidance + "\n<available_skills>\n" + strings.Join(lines, "\n") + "\n</available_skills>" + hiddenNote
	}
	return "## Skills\n" +
		"Before replying, scan the skills below. If a skill matches or is even partially relevant to your " +
		"task, you MUST load it with skill_view(name) and follow its instructions. Err on the side of " +
		"loading — it is always better to have context you don't need than to miss critical steps, pitfalls, " +
		"or established workflows. Skills contain specialized knowledge — API endpoints, tool-specific " +
		"commands, and proven workflows that outperform general-purpose approaches. Load the skill " +
		"even if you think you could handle the task with basic tools like " + basicTools + ". " +
		"Skills also encode the user's preferred approach, conventions, and quality standards for tasks like " +
		"code review, planning, and testing — load them even for tasks you already know how to do, because " +
		"the skill defines how it should be done here.\n" +
		"If a skill has issues, fix it with skill_manage(action='patch').\n" +
		"After difficult/iterative tasks, offer to save as a skill. If a skill you loaded was missing steps, " +
		"had wrong commands, or needed pitfalls you discovered, update it before finishing.\n" +
		"\n" +
		"<available_skills>\n" +
		strings.Join(lines, "\n") + "\n" +
		"</available_skills>\n\n" +
		"Only proceed without loading a skill if genuinely none are relevant to the task." +
		hiddenNote
}
