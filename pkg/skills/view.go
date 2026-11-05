package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/config"
	pyfnmatch "github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Readiness states a skill_view result reports
const (
	ReadinessAvailable   = "available"
	ReadinessSetupNeeded = "setup_needed"
	ReadinessUnsupported = "unsupported"
)

const lookupHint = "Use a skill name or relative path within the skills directory."

// Lowercase phrases whose presence in a skill logs a prompt injection warning
var injectionPatterns = []string{
	"ignore previous instructions", "ignore all previous", "you are now",
	"disregard your", "forget your instructions", "new instructions:",
	"system prompt:", "<system>", "]]>",
}

var supportDirOrder = []string{"references", "templates", "assets", "scripts"}

var skillFileExts = map[string]bool{".md": true, ".py": true, ".yaml": true, ".yml": true, ".json": true, ".tex": true, ".sh": true}

// Plugin manager surface skill listing and plugin skill serving consult
type PluginSkillHost interface {
	Discover(ctx context.Context)
	FindPluginSkill(qualified string) (string, bool)
	ListPluginSkills(namespace string) []string
	ListPluginSkillMetadata() []map[string]any
	RemovePluginSkill(qualified string)
	LegacyPortableSkillName(namespace, bare string) string
	ActiveMemoryProvider() (string, error)
	PruneInactiveMemoryProviderSkills(active string) error
	LoadMemoryProvider(name string) error
	SkillOwners(qualified string) []string
	DisabledPlugins() map[string]bool
}

var (
	pluginHostMu sync.RWMutex
	pluginHostFn func(ctx context.Context, home string) PluginSkillHost
)

// Installs the plugin skill host factory, called by pkg/plugins at init
func RegisterPluginSkillHost(fn func(ctx context.Context, home string) PluginSkillHost) {
	pluginHostMu.Lock()
	pluginHostFn = fn
	pluginHostMu.Unlock()
}

// Plugin skill host for the scope home, an error when pkg/plugins never registered one
func (s *Scope) pluginHost(ctx context.Context) (PluginSkillHost, error) {
	pluginHostMu.RLock()
	fn := pluginHostFn
	pluginHostMu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("plugin skill host is not registered")
	}
	return fn(ctx, s.Home), nil
}

// Secret capture callback that prompts for one env var and reports success, skipped and stored_as
type SecretCaptureFunc func(name, prompt string, metadata map[string]any) (map[string]any, error)

type turnSecretCaptureKey struct{}

var (
	secretCaptureMu       sync.RWMutex
	secretCaptureCallback SecretCaptureFunc
)

// Installs the process wide secret capture callback the CLI surface provides
func SetSecretCaptureCallback(cb SecretCaptureFunc) {
	secretCaptureMu.Lock()
	secretCaptureCallback = cb
	secretCaptureMu.Unlock()
}

// Binds a secret capture callback for one turn, winning over the process wide callback
func WithTurnSecretCapture(ctx context.Context, cb SecretCaptureFunc) context.Context {
	return context.WithValue(ctx, turnSecretCaptureKey{}, cb)
}

// Error when a lookup name could escape the search roots it is joined onto, empty when safe
func skillLookupPathError(name string) string {
	candidate := strings.TrimSpace(name)
	norm := strings.ReplaceAll(candidate, "/", `\`)
	winDrive := strings.HasPrefix(norm, `\\`) || (len([]rune(candidate)) >= 2 && []rune(candidate)[1] == ':')
	if strings.HasPrefix(candidate, "/") || winDrive {
		return "Skill name must be a relative path within the skills directory."
	}
	if tools.HasTraversalComponent(candidate) {
		return "Skill name cannot contain '..' path traversal components."
	}
	return ""
}

// Reports whether skills_tool style config disables any of the names, false when the config cannot be read
func (s *Scope) toolSkillDisabled(platform string, names ...string) bool {
	raw, ok := s.tree()["skills"]
	if !ok {
		raw = map[string]any{}
	}
	cfg, isMap := raw.(map[string]any)
	if !isMap {
		return false
	}
	if platform == "" {
		platform = s.Platform
	}
	var platformDisabled any
	hasPlatform := false
	if platform != "" {
		if pd, ok := cfg["platform_disabled"].(map[string]any); ok {
			platformDisabled, hasPlatform = pd[platform]
			hasPlatform = hasPlatform && platformDisabled != nil
		}
	}
	disabled, ok := cfg["disabled"]
	if !ok {
		disabled = []any{}
	}
	for _, n := range names {
		if hasPlatform {
			in, err := pyContains(platformDisabled, n)
			if err != nil {
				return false
			}
			if in {
				return true
			}
		}
		in, err := pyContains(disabled, n)
		if err != nil {
			return false
		}
		if in {
			return true
		}
	}
	return false
}

// Python in operator of a string over a config value, an error where Python raises TypeError
func pyContains(container any, n string) (bool, error) {
	switch c := container.(type) {
	case string:
		return strings.Contains(c, n), nil
	case []any:
		for _, item := range c {
			if s, ok := item.(string); ok && s == n {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		_, ok := c[n]
		return ok, nil
	}
	return false, fmt.Errorf("argument of type '%T' is not iterable", container)
}

// Tier one listing of loadable and plugin skills with categories
func (s *Scope) SkillsList(ctx context.Context, category string) string {
	if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
		return failResult(err.Error(), nil)
	}
	all := s.FindAllSkills(false)
	if host, err := s.pluginHost(ctx); err != nil {
		s.logger().Debug("Plugin skill listing failed", "error", err)
	} else {
		host.Discover(ctx)
		for _, ps := range host.ListPluginSkillMetadata() {
			fm, _ := ps["frontmatter"].(map[string]any)
			delete(ps, "frontmatter")
			if fm == nil {
				fm = map[string]any{}
			}
			if !skillMatchesPlatform(fm) || s.toolSkillDisabled("", fmString(ps["name"])) {
				continue
			}
			all = append(all, ps)
		}
	}
	if len(all) == 0 {
		return jsonResult(map[string]any{"success": true, "skills": []any{}, "categories": []any{}, "message": "No skills found in skills/ directory."})
	}
	if category != "" {
		var kept []map[string]any
		for _, sk := range all {
			if c, ok := sk["category"].(string); ok && c == category {
				kept = append(kept, sk)
			}
		}
		all = kept
	}
	all = SortSkills(all)
	catSet := map[string]bool{}
	for _, sk := range all {
		if c := fmString(sk["category"]); c != "" {
			catSet[c] = true
		}
	}
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	if all == nil {
		all = []map[string]any{}
	}
	return jsonResult(map[string]any{"success": true, "skills": all, "categories": cats, "count": len(all),
		"hint": "Use skill_view(name) to see full content, tags, and linked files"})
}

// Plugin namespace dispatch, answered with a result or falling through with the on disk category form
func (s *Scope) resolvePluginSkill(ctx context.Context, name, filePath, taskID string, preprocess bool) (string, string, error) {
	namespace, bare, _ := parseQualifiedName(name)
	if namespace == "plugin" && strings.Contains(bare, ":") {
		name = bare
		namespace, bare, _ = parseQualifiedName(name)
	}
	if !isValidNamespace(namespace) {
		return failResult(fmt.Sprintf("Invalid namespace '%s' in '%s'. Namespaces must match [a-zA-Z0-9_-]+.", namespace, name), nil), "", nil
	}
	host, err := s.pluginHost(ctx)
	if err != nil {
		return "", "", err
	}
	host.Discover(ctx)
	if legacy := host.LegacyPortableSkillName(namespace, bare); legacy != "" {
		name = legacy
		namespace, _, _ = strings.Cut(legacy, ":")
	}
	active, err := host.ActiveMemoryProvider()
	if err == nil {
		err = host.PruneInactiveMemoryProviderSkills(active)
	}
	if err != nil {
		s.logger().Debug(fmt.Sprintf("Failed pruning inactive memory-provider skills: %v", err))
	}
	skillMD, found := host.FindPluginSkill(name)
	if !found && active != "" && namespace == active {
		if lerr := host.LoadMemoryProvider(namespace); lerr != nil {
			s.logger().Debug(fmt.Sprintf("Failed lazy memory-provider skill load for %s: %v", namespace, lerr))
		} else {
			skillMD, found = host.FindPluginSkill(name)
		}
	}
	if found && !lexistsFollow(skillMD) {
		host.RemovePluginSkill(name)
		return failResult(fmt.Sprintf("Skill '%s' file no longer exists at %s. The registry entry has been cleaned up — try again after the plugin is reloaded.", name, skillMD), nil), "", nil
	}
	if found {
		return s.servePluginSkill(ctx, host, skillMD, namespace, bare, filePath, preprocess, taskID), "", nil
	}
	if available := host.ListPluginSkills(namespace); len(available) > 0 {
		qualified := make([]string, len(available))
		for i, a := range available {
			qualified[i] = namespace + ":" + a
		}
		return failResult(fmt.Sprintf("Skill '%s' not found in plugin '%s'.", bare, namespace), map[string]any{
			"available_skills": qualified, "hint": fmt.Sprintf("The '%s' plugin provides %d skill(s).", namespace, len(available))}), "", nil
	}
	if bare != "" {
		return "", namespace + "/" + bare, nil
	}
	return "", "", nil
}

// Reports whether the resolved path lives under one of the dirs
func underAny(p string, dirs []string) bool {
	resolved := resolvePath(p)
	for _, d := range dirs {
		if isRelativeTo(resolved, d) {
			return true
		}
	}
	return false
}

// Reports whether a legacy markdown candidate belongs to an ancestor directory skill
func isPackageOwnedMarkdown(p, searchRoot string) bool {
	if !isRelativeTo(p, searchRoot) {
		return false
	}
	rel, err := filepath.Rel(searchRoot, p)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for depth := 1; depth < len(parts); depth++ {
		if isFile(filepath.Join(append([]string{searchRoot}, append(parts[:depth:depth], "SKILL.md")...)...)) {
			return true
		}
	}
	return false
}

// One located skill as its directory, empty for a flat markdown skill, and its markdown file
type skillLocation struct {
	dir string
	md  string
}

// Paths under root whose base name matches pattern, symlinked dirs matched but never descended
func rglob(root, pattern string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != root && pyfnmatch.FnmatchCase(d.Name(), pattern) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Paths directly inside dir whose base name matches pattern
func globDir(dir, pattern string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if pyfnmatch.FnmatchCase(e.Name(), pattern) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// Strips the final extension of a path the way Path.with_suffix does
func withSuffix(p, suffix string) string {
	base := filepath.Base(p)
	if ext := filepath.Ext(base); ext != "" && ext != base {
		return p[:len(p)-len(ext)] + suffix
	}
	return p + suffix
}

// Every candidate across all dirs and lookup strategies deduplicated by resolved path
func (s *Scope) collectSkillCandidates(name, localCategory string, allDirs []string) []skillLocation {
	var cands []skillLocation
	seen := map[string]bool{}
	record := func(dir, md string) {
		if key := resolvePath(md); !seen[key] {
			seen[key] = true
			cands = append(cands, skillLocation{dir, md})
		}
	}
	recordDirect := func(direct, searchRoot string) {
		flat := withSuffix(direct, ".md")
		if !isSkillSupportPath(direct, "") && isDir(direct) && lexistsFollow(filepath.Join(direct, "SKILL.md")) {
			record(direct, filepath.Join(direct, "SKILL.md"))
		} else if lexistsFollow(flat) && !isSkillSupportPath(flat, "") && !isPackageOwnedMarkdown(flat, searchRoot) {
			record("", flat)
		}
	}
	for _, searchDir := range allDirs {
		for _, direct := range []string{name, localCategory} {
			if direct != "" {
				recordDirect(filepath.Join(searchDir, direct), searchDir)
			}
		}
		for _, found := range iterSkillIndexFiles(searchDir, "SKILL.md") {
			if filepath.Base(filepath.Dir(found)) == name || fmEquals(safeFrontmatterPath(found)["name"], name) {
				record(filepath.Dir(found), found)
			}
		}
		for _, found := range rglob(searchDir, name+".md") {
			if filepath.Base(found) != "SKILL.md" && !isSkillSupportPath(found, "") && !isPackageOwnedMarkdown(found, searchDir) {
				record("", found)
			}
		}
	}
	return cands
}

// Reports whether a frontmatter value equals a string the way Python equality does
func fmEquals(v any, want string) bool {
	s, ok := v.(string)
	return ok && s == want
}

// Frontmatter of a file, empty on any read or parse failure
func safeFrontmatterPath(p string) map[string]any {
	text, err := readSkillText(p)
	if err != nil {
		return map[string]any{}
	}
	return safeFrontmatter(text)
}

// References, templates, assets and scripts of a directory skill with empty groups dropped
func skillLinkedFiles(skillDir string) map[string]any {
	files := map[string]any{}
	if skillDir == "" {
		return files
	}
	specs := []struct {
		sub       string
		globs     []string
		recursive bool
		filesOnly bool
	}{
		{"references", []string{"*.md"}, false, false},
		{"templates", []string{"*.md", "*.py", "*.yaml", "*.yml", "*.json", "*.tex", "*.sh"}, true, false},
		{"assets", []string{"*"}, true, true},
		{"scripts", []string{"*.py", "*.sh", "*.bash", "*.js", "*.ts", "*.rb"}, false, false},
	}
	for _, sp := range specs {
		base := filepath.Join(skillDir, sp.sub)
		if !lexistsFollow(base) {
			continue
		}
		var found []string
		for _, g := range sp.globs {
			var matches []string
			if sp.recursive {
				matches = rglob(base, g)
			} else {
				matches = globDir(base, g)
			}
			for _, f := range matches {
				if sp.filesOnly && !isFile(f) {
					continue
				}
				if rel, err := filepath.Rel(skillDir, f); err == nil {
					found = append(found, filepath.ToSlash(rel))
				}
			}
		}
		if len(found) > 0 {
			files[sp.sub] = found
		}
	}
	return files
}

// Most specific search dir lexically containing the markdown file, empty when none does
func owningSearchDir(skillMD string, allDirs []string) string {
	best, bestParts := "", -1
	for _, d := range allDirs {
		if isRelativeTo(skillMD, d) {
			if n := len(strings.Split(filepath.ToSlash(filepath.Clean(d)), "/")); n > bestParts {
				best, bestParts = d, n
			}
		}
	}
	return best
}

// Exact load path of a candidate relative to its owning root
func ownedRelative(loc skillLocation, allDirs []string) string {
	root := owningSearchDir(loc.md, allDirs)
	target := loc.dir
	if target == "" {
		target = withSuffix(loc.md, "")
	}
	if root == "" {
		return target
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

// Unique on disk skill for a name under the roots, else the refusal or not found result
func (s *Scope) locateSkill(name, localCategory string, roots []skillRoot) (string, skillLocation) {
	allDirs := make([]string, len(roots))
	tierOf := map[string]int{}
	for i, r := range roots {
		allDirs[i] = r.dir
		if _, ok := tierOf[r.dir]; !ok {
			tierOf[r.dir] = r.tier
		}
	}
	if len(allDirs) == 0 {
		return failResult("Skills directory does not exist yet. It will be created on first install.", nil), skillLocation{}
	}
	cands := s.collectSkillCandidates(name, localCategory, allDirs)
	if len(cands) > 1 {
		pc := make([]skillCandidate, len(cands))
		for i, c := range cands {
			root := owningSearchDir(c.md, allDirs)
			pc[i] = skillCandidate{tierOf[root], root, skillCandidateRank(c.md, root), c.md}
		}
		won, contenders := pickSkillCandidate(pc)
		if won >= 0 {
			var dropped []string
			for i, c := range cands {
				if i != won {
					dropped = append(dropped, c.md)
				}
			}
			if len(dropped) > 0 {
				s.logger().Info(fmt.Sprintf("Skill '%s' resolved to %s by precedence (project > local > create_dir > external_dirs, then identical same-root copies); not loaded: %s",
					name, cands[won].md, strings.Join(dropped, "; ")))
			}
			cands = []skillLocation{cands[won]}
		} else {
			kept := make([]skillLocation, 0, len(contenders))
			for _, i := range contenders {
				kept = append(kept, cands[i])
			}
			cands = kept
		}
	}
	if len(cands) > 1 {
		paths := make([]string, len(cands))
		nameSet := map[string]bool{}
		for i, c := range cands {
			paths[i] = c.md
			if rel := ownedRelative(c, allDirs); rel != name {
				nameSet[rel] = true
			}
		}
		loadNames := make([]string, 0, len(nameSet))
		for n := range nameSet {
			loadNames = append(loadNames, n)
		}
		sort.Strings(loadNames)
		s.logger().Warn(fmt.Sprintf("Skill name collision for '%s': %d candidates — %s", name, len(cands), strings.Join(paths, "; ")))
		useOne := ""
		if len(loadNames) > 0 {
			useOne = "use one of " + strings.Join(loadNames, ", ") + ". "
		}
		return failResult(fmt.Sprintf("%s'%s': %s%d different skills share this name in the same skills directory tier; refusing to guess.", AmbiguousSkillPrefix, name, useOne, len(cands)),
			map[string]any{"matches": paths, "load_names": loadNames,
				"hint": "Pass the exact relative path instead of the bare name, or rename one of the colliding skills so each name is unique."}), skillLocation{}
	}
	var loc skillLocation
	if len(cands) == 1 {
		loc = cands[0]
	}
	if loc.md != "" {
		if t, ok := tierOf[owningSearchDir(loc.md, allDirs)]; ok && t == TierProject && s.isQuarantinedProjectSkill(loc.md) {
			return failResult(fmt.Sprintf("Project skill '%s' is quarantined: the security scan flagged its content as dangerous. It will not load until the repo's skill content changes and passes a re-scan.", name),
				map[string]any{"hint": "Inspect the skill in the repo checkout, or untrust the repo with `icebath skills untrust`."}), skillLocation{}
		}
	}
	if loc.md == "" || !lexistsFollow(loc.md) {
		available := []string{}
		for i, sk := range SortSkills(s.FindAllSkills(false)) {
			if i >= 20 {
				break
			}
			available = append(available, fmString(sk["name"]))
		}
		return failResult(fmt.Sprintf("Skill '%s' not found.", name), map[string]any{"available_skills": available, "hint": "Use skills_list to see all available skills"}), skillLocation{}
	}
	return "", loc
}

// Warns when a loaded skill resolves outside every trusted root or carries injection phrases
func (s *Scope) logSecurityWarnings(name, skillMD, content string, allDirs []string, active string) {
	trusted := []string{resolvePath(active)}
	for _, d := range allDirs {
		trusted = append(trusted, resolvePath(d))
	}
	var warnings []string
	if !underAny(skillMD, trusted) {
		warnings = append(warnings, "skill file is outside the trusted skills directory (~/.icebath/skills/): "+skillMD)
	}
	if containsInjection(content) {
		warnings = append(warnings, "skill content contains patterns that may indicate prompt injection")
	}
	if len(warnings) > 0 {
		s.logger().Warn(fmt.Sprintf("Skill security warning for '%s': %s", name, strings.Join(warnings, "; ")))
	}
}

// Reports whether lowercased content holds a prompt injection phrase
func containsInjection(content string) bool {
	lower := strings.ToLower(content)
	for _, p := range injectionPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// Skill markdown or one of its linked files as the skill_view JSON result
func (s *Scope) SkillView(ctx context.Context, name, filePath, taskID string, preprocess bool) string {
	if e := skillLookupPathError(name); e != "" {
		return failResult(e, map[string]any{"hint": lookupHint})
	}
	localCategory := ""
	if strings.Contains(name, ":") {
		served, local, err := s.resolvePluginSkill(ctx, name, filePath, taskID, preprocess)
		if err != nil {
			return failResult(err.Error(), nil)
		}
		if served != "" {
			return served
		}
		localCategory = local
	}
	if localCategory != "" {
		if e := skillLookupPathError(localCategory); e != "" {
			return failResult(e, map[string]any{"hint": lookupHint})
		}
	}
	roots, active := s.skillSearchDirs()
	allDirs := make([]string, len(roots))
	for i, r := range roots {
		allDirs[i] = r.dir
	}
	errResult, loc := s.locateSkill(name, localCategory, roots)
	if errResult != "" {
		return errResult
	}
	content, err := readSkillText(loc.md)
	if err != nil {
		return failResult(fmt.Sprintf("Failed to read skill '%s': %s", name, pyOSErrorText(err)), nil)
	}
	s.logSecurityWarnings(name, loc.md, content, allDirs, active)
	fm := safeFrontmatter(content)
	if !skillMatchesPlatform(fm) {
		return failResult(fmt.Sprintf("Skill '%s' is not supported on this platform.", name), map[string]any{"readiness_status": ReadinessUnsupported})
	}
	resolvedName := any(filepath.Base(filepath.Dir(loc.md)))
	if v, ok := fm["name"]; ok {
		resolvedName = v
	}
	rel := ownedRelative(loc, allDirs)
	rn := fmString(resolvedName)
	if s.toolSkillDisabled("", rn, rel) && (s.toolSkillDisabled("", rn) || s.catalogLoadsAs(loc.md, rel)) {
		return failResult(fmt.Sprintf("Skill '%s' is disabled. Enable it with `icebath skills` or inspect the files directly on disk.", rn), nil)
	}
	if filePath != "" && loc.dir != "" {
		return s.serveSkillFile(loc.dir, filePath, name, "Use a relative path within the skill directory", true, false, true)
	}
	metadata, metaIsMap := fm["metadata"].(map[string]any)
	meta := icebathMeta(fm)
	tagSource := func(k string) any {
		if v := meta[k]; fmTruthy(v) {
			return v
		}
		if v, ok := fm[k]; ok {
			return v
		}
		return ""
	}
	tags, related := parseTags(tagSource("tags")), parseTags(tagSource("related_skills"))
	linked := skillLinkedFiles(loc.dir)
	relPath := ""
	if isRelativeTo(loc.md, active) {
		relPath, _ = filepath.Rel(active, loc.md)
	} else {
		relPath, _ = filepath.Rel(filepath.Dir(filepath.Dir(loc.md)), loc.md)
	}
	skillName := any(filepath.Base(loc.dir))
	if loc.dir == "" {
		skillName = filepath.Base(withSuffix(loc.md, ""))
	}
	if v, ok := fm["name"]; ok {
		skillName = v
	}
	readiness := s.skillReadiness(ctx, fm, fmString(skillName))
	rendered := content
	if preprocess {
		rendered = s.PreprocessSkillContent(ctx, content, loc.dir, taskID, nil)
	}
	result := map[string]any{
		"success": true, "name": skillName, "description": fmGet(fm, "description", ""),
		"tags": tags, "related_skills": related, "content": rendered, "path": relPath,
		"skill_dir": nil, "linked_files": nil, "usage_hint": nil,
	}
	if loc.dir != "" {
		result["skill_dir"] = loc.dir
	}
	if len(linked) > 0 {
		result["linked_files"] = linked
		result["usage_hint"] = "To view linked files, call skill_view(name, file_path) where file_path is e.g. 'references/api.md' or 'assets/config.yaml'"
	}
	for k, v := range readiness {
		result[k] = v
	}
	if note := s.ensureDeclaredDeps(ctx, fm); note != "" {
		result["deps_note"] = note
	}
	markBackgroundReviewSkillRead(s.Env, loc.md)
	if fmTruthy(fm["compatibility"]) {
		result["compatibility"] = fm["compatibility"]
	}
	if metaIsMap {
		result["metadata"] = metadata
	}
	return jsonResult(result)
}

// Frontmatter value with a default for an absent key
func fmGet(fm map[string]any, key string, def any) any {
	if v, ok := fm[key]; ok {
		return v
	}
	return def
}

// Reports whether the unfiltered catalog loads this exact file under the relative name
func (s *Scope) catalogLoadsAs(skillMD, rel string) bool {
	for _, e := range s.skillCatalog(true, true) {
		if e.Path == skillMD && e.LoadName == rel {
			return true
		}
	}
	return false
}

// Installs declared deps through the package manager, the failure note naming the install remedy
func (s *Scope) ensureDeclaredDeps(ctx context.Context, fm map[string]any) string {
	declared := fm["deps"]
	if !fmTruthy(declared) {
		return ""
	}
	if str, ok := declared.(string); ok {
		declared = []any{str}
	}
	list, ok := declared.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	var failed, all []string
	for _, d := range list {
		all = append(all, pyStrValue(d))
		dep := strings.TrimSpace(pyStrValue(d))
		if dep == "" {
			continue
		}
		if _, err := pm.Ensure(ctx, dep, pm.EnsureOptions{}); err != nil {
			failed = append(failed, dep+": "+err.Error())
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return "Tool dependencies could not be installed — " + strings.Join(failed, "; ") + ". Run `icebath pm install " + strings.Join(all, " ") + "` and reload."
}

// Python str of a YAML scalar, True and False capitalized and None spelled out
func pyStrValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return fmString(v)
}

// Text Python prints for an OSError reading a file
func pyOSErrorText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		msg := pe.Err.Error()
		switch {
		case os.IsNotExist(err):
			msg = "No such file or directory"
		case os.IsPermission(err):
			msg = "Permission denied"
		}
		code := 0
		var errno syscall.Errno
		if errors.As(pe.Err, &errno) {
			code = int(errno)
		}
		return fmt.Sprintf("[Errno %d] %s: '%s'", code, msg, pe.Path)
	}
	return err.Error()
}

var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var remoteEnvBackends = map[string]bool{"docker": true, "modal": true, "ssh": true, "daytona": true, "vercel_sandbox": true}

// Reports built in remote backends plus plugin backends declaring themselves remote
func isRemoteEnvBackend(backend string) bool {
	if remoteEnvBackends[backend] || backend == "" || backend == "local" {
		return remoteEnvBackends[backend]
	}
	p := tools.GetTerminalBackend(backend)
	return p != nil && p.IsRemote()
}

// A single mapping or a list as a list, anything else empty
func asDictList(raw any) []any {
	switch t := raw.(type) {
	case map[string]any:
		return []any{t}
	case []any:
		return t
	}
	return nil
}

// Stripped string of a non blank string value, empty otherwise
func cleanStr(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// First truthy value among the candidates, nil when every one is falsy
func firstTruthy(vals ...any) any {
	for _, v := range vals {
		if fmTruthy(v) {
			return v
		}
	}
	return nil
}

// Required env vars merged from required_environment_variables, setup.collect_secrets and prerequisites.env_vars
func getRequiredEnvironmentVariables(fm map[string]any) []map[string]any {
	setup, _ := fm["setup"].(map[string]any)
	if setup == nil {
		setup = map[string]any{}
	}
	setupHelp := cleanStr(setup["help"])
	var legacyRaw any
	if prereqs, ok := fm["prerequisites"].(map[string]any); ok {
		legacyRaw = prereqs["env_vars"]
	}
	var legacy []any
	if fmTruthy(legacyRaw) {
		switch t := legacyRaw.(type) {
		case string:
			legacy = []any{t}
		case []any:
			legacy = t
		case map[string]any:
			for k := range t {
				legacy = append(legacy, k)
			}
		}
	}
	var entries []map[string]any
	for _, item := range asDictList(fm["required_environment_variables"]) {
		switch t := item.(type) {
		case string:
			entries = append(entries, map[string]any{"name": t})
		case map[string]any:
			entries = append(entries, t)
		}
	}
	for _, item := range asDictList(setup["collect_secrets"]) {
		if m, ok := item.(map[string]any); ok {
			var url any
			if u := strings.TrimSpace(pyStrOr(firstTruthy(m["provider_url"], m["url"]))); u != "" {
				url = u
			}
			entries = append(entries, map[string]any{"name": m["env_var"], "prompt": m["prompt"], "url": url})
		}
	}
	for _, v := range legacy {
		if strings.TrimSpace(pyStrValue(v)) != "" {
			entries = append(entries, map[string]any{"name": pyStrValue(v)})
		}
	}
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, e := range entries {
		envName := strings.TrimSpace(pyStrOr(firstTruthy(e["name"], e["env_var"])))
		if envName == "" || seen[envName] || !envVarNameRe.MatchString(envName) {
			continue
		}
		prompt := "Enter value for " + envName
		if p := firstTruthy(e["prompt"]); p != nil {
			prompt = pyStrValue(p)
		}
		norm := map[string]any{"name": envName, "prompt": strings.TrimSpace(prompt)}
		helpRaw := firstTruthy(e["help"], e["provider_url"], e["url"])
		if helpRaw == nil && setupHelp != "" {
			helpRaw = setupHelp
		}
		if h := cleanStr(helpRaw); h != "" {
			norm["help"] = h
		}
		if rf := cleanStr(e["required_for"]); rf != "" {
			norm["required_for"] = rf
		}
		if fmTruthy(e["optional"]) {
			norm["optional"] = true
		}
		seen[envName] = true
		out = append(out, norm)
	}
	return out
}

// Python str of a value with None mapping to an empty string
func pyStrOr(v any) string {
	if v == nil {
		return ""
	}
	return pyStrValue(v)
}

// Prompts for missing secrets through the bound capture callback, reporting what stays missing
func (s *Scope) captureRequiredEnvironmentVariables(ctx context.Context, skillName string, missing []map[string]any) ([]string, bool, any) {
	if len(missing) == 0 {
		return []string{}, false, nil
	}
	names := make([]string, len(missing))
	for i, e := range missing {
		names[i] = e["name"].(string)
	}
	cb, _ := ctx.Value(turnSecretCaptureKey{}).(SecretCaptureFunc)
	if cb == nil && s.isGatewaySurface() && !envEnabled("ICEBATH_INTERACTIVE") {
		return names, false, "Secure secret entry is not supported over messaging. Load this skill in the local CLI to be prompted, or add the key to ~/.icebath/.env manually."
	}
	if cb == nil {
		secretCaptureMu.RLock()
		cb = secretCaptureCallback
		secretCaptureMu.RUnlock()
	}
	if cb == nil {
		return names, false, nil
	}
	remaining := []string{}
	for _, e := range missing {
		meta := map[string]any{"skill_name": skillName}
		for _, k := range []string{"help", "required_for"} {
			if fmTruthy(e[k]) {
				meta[k] = e[k]
			}
		}
		res, err := cb(e["name"].(string), e["prompt"].(string), meta)
		if err != nil {
			s.logger().Warn(fmt.Sprintf("Secret capture callback failed for %s", e["name"]), "error", err)
			res = map[string]any{"success": false, "stored_as": e["name"], "validated": false, "skipped": true}
		}
		if !(res != nil && fmTruthy(res["success"]) && !fmTruthy(res["skipped"])) {
			remaining = append(remaining, e["name"].(string))
		}
	}
	return remaining, len(remaining) > 0, nil
}

// Reports whether a process env flag spells true
func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Reports a gateway session or one bound to a messaging platform
func (s *Scope) isGatewaySurface() bool {
	if s.Env != nil && s.Env.Values != nil {
		if v, ok := s.Env.Values.Load("session.gateway"); ok {
			if b, ok := v.(bool); ok && b {
				return true
			}
		}
	}
	if envEnabled("ICEBATH_GATEWAY_SESSION") {
		return true
	}
	if s.Env != nil && s.Env.Values != nil {
		if v, ok := s.Env.Values.Load("session.platform"); ok {
			if p, ok := v.(string); ok {
				return p != ""
			}
		}
	}
	return os.Getenv("ICEBATH_SESSION_PLATFORM") != ""
}

// Profile secret lookup pkg/provider installs at init, an error meaning an unscoped read while multiplexing
var secretResolver func(ctx context.Context, name string) (string, bool, error)

// Installs the secret_scope.get_secret port, pkg/provider passes its GetSecret
func RegisterSecretResolver(fn func(ctx context.Context, name string) (string, bool, error)) {
	secretResolver = fn
}

// Reports whether a requirement is in the env snapshot or resolves through the active profile secret scope
func (s *Scope) isEnvVarPersisted(ctx context.Context, name string, snapshot map[string]string) bool {
	if v, ok := snapshot[name]; ok {
		return v != ""
	}
	if secretResolver == nil {
		panic("skills: pkg/provider must call RegisterSecretResolver(GetSecret) at init before skill readiness is checked")
	}
	v, _, err := secretResolver(ctx, name)
	return err == nil && v != ""
}

// Setup note for a skill needing setup, empty otherwise
func buildSetupNote(status string, missing []string, setupHelp string) string {
	if status != ReadinessSetupNeeded {
		return ""
	}
	what := "required prerequisites"
	if len(missing) > 0 {
		what = strings.Join(missing, ", ")
	}
	note := "Setup needed before using this skill: missing " + what + "."
	if setupHelp != "" {
		return note + " " + setupHelp
	}
	return note
}

// Profile env file snapshot, empty for a frozen policy worker that never opens it
func (s *Scope) loadEnvSnapshot() map[string]string {
	if config.WorkerConfigSnapshot() != nil {
		return map[string]string{}
	}
	return s.Secrets.File()
}

// Tool env the sandbox registries key on, a scope only env when no tool call bound one
func (s *Scope) registrationEnv() *tools.Env {
	if s.Env != nil {
		return s.Env
	}
	return &tools.Env{Home: s.Home, Config: s.tree(), Secrets: s.Secrets, Platform: s.Platform, Cwd: s.Cwd, Log: s.Log}
}

// Required env vars and credential files resolved with secret capture and sandbox registration
func (s *Scope) skillReadiness(ctx context.Context, fm map[string]any, skillName string) map[string]any {
	required := getRequiredEnvironmentVariables(fm)
	backend := "local"
	if policy, err := tools.BuildTerminalScope(s.Home, s.Secrets); err == nil {
		if b := strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_ENV", "local"))); b != "" {
			backend = b
		}
	}
	snapshot := s.loadEnvSnapshot()
	var missingReq []map[string]any
	for _, e := range required {
		if !fmTruthy(e["optional"]) && !s.isEnvVarPersisted(ctx, e["name"].(string), snapshot) {
			missingReq = append(missingReq, e)
		}
	}
	stillMissing, skipped, gatewayHint := s.captureRequiredEnvironmentVariables(ctx, skillName, missingReq)
	if len(missingReq) > 0 {
		snapshot = s.loadEnvSnapshot()
	}
	still := map[string]bool{}
	for _, n := range stillMissing {
		still[n] = true
	}
	remaining := []string{}
	remSet := map[string]bool{}
	for _, e := range required {
		n := e["name"].(string)
		if !fmTruthy(e["optional"]) && (still[n] || !s.isEnvVarPersisted(ctx, n, snapshot)) {
			remaining = append(remaining, n)
			remSet[n] = true
		}
	}
	setupNeeded := len(remaining) > 0
	var available []string
	for _, e := range required {
		if n := e["name"].(string); !remSet[n] {
			available = append(available, n)
		}
	}
	if len(available) > 0 {
		tools.RegisterEnvPassthrough(s.registrationEnv(), available)
	}
	missingCred := []string{}
	if raw, ok := fm["required_credential_files"].([]any); ok && len(raw) > 0 {
		if m := tools.RegisterCredentialFiles(s.registrationEnv(), raw, "/root/.icebath"); len(m) > 0 {
			missingCred = m
		}
		setupNeeded = setupNeeded || len(missingCred) > 0
	}
	status := ReadinessAvailable
	if setupNeeded {
		status = ReadinessSetupNeeded
	}
	reqOut := make([]any, len(required))
	for i, e := range required {
		reqOut[i] = e
	}
	out := map[string]any{
		"required_environment_variables": reqOut, "required_commands": []any{},
		"missing_required_environment_variables": remaining, "missing_credential_files": missingCred,
		"missing_required_commands": []any{}, "setup_needed": setupNeeded, "setup_skipped": skipped,
		"readiness_status": status,
	}
	setupHelp := ""
	for _, e := range required {
		if h, ok := e["help"].(string); ok && h != "" {
			setupHelp = h
			break
		}
	}
	if setupHelp != "" {
		out["setup_help"] = setupHelp
	}
	if fmTruthy(gatewayHint) {
		out["gateway_setup_hint"] = gatewayHint
	}
	var items []string
	for _, n := range remaining {
		items = append(items, "env $"+n)
	}
	for _, p := range missingCred {
		items = append(items, "file "+p)
	}
	if setupNeeded {
		if note := buildSetupNote(status, items, setupHelp); note != "" {
			if isRemoteEnvBackend(backend) {
				note += " " + strings.ToUpper(backend) + "-backed skills need these requirements available inside the remote environment as well."
			}
			out["setup_note"] = note
		}
	}
	return out
}

// Non SKILL.md files grouped by support dir plus other for known source extensions
func availableSkillFiles(skillDir string) map[string]any {
	groups := map[string][]string{}
	filepath.WalkDir(skillDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == skillDir || !isFile(p) || d.Name() == "SKILL.md" {
			return nil
		}
		rel, rerr := filepath.Rel(skillDir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		top := ""
		if i := strings.Index(rel, "/"); i >= 0 {
			top = rel[:i]
		}
		isSupport := SkillSupportDirs[top]
		if isSupport || skillFileExts[pathSuffix(p)] {
			key := "other"
			if isSupport {
				key = top
			}
			groups[key] = append(groups[key], rel)
		}
		return nil
	})
	out := map[string]any{}
	for _, k := range append(append([]string(nil), supportDirOrder...), "other") {
		if g, ok := groups[k]; ok {
			out[k] = g
		}
	}
	return out
}

// Final suffix of a path the way PurePath.suffix reports it
func pathSuffix(p string) string {
	base := filepath.Base(p)
	if ext := filepath.Ext(base); ext != base && ext != "." {
		return ext
	}
	return ""
}

// One linked file of a skill directory as a skill_view result
func (s *Scope) serveSkillFile(skillRoot, filePath, label, hint string, listAvailable, readErrorPrefix, markRead bool) string {
	extra := map[string]any{}
	if hint != "" {
		extra["hint"] = hint
	}
	if tools.HasTraversalComponent(filePath) {
		return failResult("Path traversal ('..') is not allowed.", extra)
	}
	target := filepath.Join(skillRoot, filePath)
	if e := tools.ValidateWithinDir(target, skillRoot); e != "" {
		return failResult(e, extra)
	}
	if !isFile(target) {
		listing := map[string]any{}
		if listAvailable {
			listing = map[string]any{"available_files": availableSkillFiles(skillRoot), "hint": "Use one of the available file paths listed above"}
		}
		return failResult(fmt.Sprintf("File '%s' not found in skill '%s'.", filePath, label), listing)
	}
	content, err := readSkillText(target)
	if err != nil {
		if !readErrorPrefix {
			return failResult(pyOSErrorText(err), nil)
		}
		return failResult(fmt.Sprintf("Failed to read '%s': %s", filePath, pyOSErrorText(err)), nil)
	}
	if markRead {
		markBackgroundReviewSkillRead(s.Env, target)
	}
	return jsonResult(map[string]any{"success": true, "name": label, "file": filePath, "content": content, "file_type": pathSuffix(target)})
}

// Plugin provided skill with disabled, platform and injection guards plus its bundle banner
func (s *Scope) servePluginSkill(ctx context.Context, host PluginSkillHost, skillMD, namespace, bare, filePath string, preprocess bool, sessionID string) string {
	qualified := namespace + ":" + bare
	disabledPlugins := host.DisabledPlugins()
	var disabled []string
	for _, owner := range host.SkillOwners(qualified) {
		if disabledPlugins[owner] {
			disabled = append(disabled, owner)
		}
	}
	sort.Strings(disabled)
	if len(disabled) > 0 {
		return failResult(fmt.Sprintf("Plugin '%s' is disabled. Re-enable with: icebath plugins enable %s", disabled[0], disabled[0]), nil)
	}
	content, err := readSkillText(skillMD)
	if err != nil {
		return failResult(fmt.Sprintf("Failed to read skill '%s': %s", qualified, pyOSErrorText(err)), nil)
	}
	fm := safeFrontmatter(content)
	if s.toolSkillDisabled("", qualified) {
		return failResult(fmt.Sprintf("Skill '%s' is disabled.", qualified), nil)
	}
	if !skillMatchesPlatform(fm) {
		return failResult(fmt.Sprintf("Skill '%s' is not supported on this platform.", qualified), map[string]any{"readiness_status": ReadinessUnsupported})
	}
	if filePath != "" {
		return s.serveSkillFile(filepath.Dir(skillMD), filePath, qualified, "", false, true, false)
	}
	if containsInjection(content) {
		s.logger().Warn(fmt.Sprintf("Plugin skill '%s:%s' contains patterns that may indicate prompt injection", namespace, bare))
	}
	var siblings []string
	for _, sk := range host.ListPluginSkills(namespace) {
		if sk != bare {
			siblings = append(siblings, sk)
		}
	}
	banner := "[Bundle context: This skill is part of the '" + namespace + "' plugin."
	if len(siblings) > 0 {
		banner += "\nSibling skills: " + strings.Join(siblings, ", ") + ".\nUse qualified form to invoke siblings (e.g. " + namespace + ":" + siblings[0] + ")."
	}
	banner += "]\n\n"
	rendered := content
	if preprocess {
		rendered = s.PreprocessSkillContent(ctx, content, filepath.Dir(skillMD), sessionID, nil)
	}
	var linked any
	if l := pluginSkillLinkedFiles(filepath.Dir(skillMD)); len(l) > 0 {
		linked = l
	}
	return jsonResult(map[string]any{
		"success": true, "name": qualified, "content": banner + rendered,
		"description":  truncateDescription(pyStrValue(fmGet(fm, "description", ""))),
		"linked_files": linked, "readiness_status": ReadinessAvailable, "skill_dir": filepath.Dir(skillMD),
	})
}

// Sorted support files of a plugin skill contained in its root, empty groups dropped
func pluginSkillLinkedFiles(root string) map[string]any {
	linked := map[string]any{}
	for _, cat := range supportDirOrder {
		var files []string
		for _, p := range rglob(filepath.Join(root, cat), "*") {
			if isFile(p) && tools.ValidateWithinDir(p, root) == "" {
				if rel, err := filepath.Rel(root, p); err == nil {
					files = append(files, rel)
				}
			}
		}
		sort.Strings(files)
		if len(files) > 0 {
			linked[cat] = files
		}
	}
	return linked
}

// Repeat view stub text served when a task already holds identical skill content
const skillViewDedupMessage = "Skill content unchanged since it was loaded earlier in this conversation — refer to the earlier skill_view result; it is still current and complete. (Re-issued after context compression, this returns the full content again.)"

const skillViewDedupCap = 200

// Per task FIFO of served skill view digests keyed by resolved name and file path
type dedupCache struct {
	order   []string
	digests map[string]string
}

var (
	skillViewTrackerMu sync.Mutex
	skillViewTracker   = map[string]*dedupCache{}
)

// Unchanged stub when the task was already served exactly this result, else records its digest
func dedupSkillView(taskID, name, filePath string, payload map[string]any, result string) string {
	if taskID == "" || fmTruthy(payload["setup_needed"]) || payload["readiness_status"] == ReadinessSetupNeeded {
		return ""
	}
	resolved := name
	if v := payload["name"]; fmTruthy(v) {
		resolved = pyStrValue(v)
	}
	key := resolved + "\x00" + filePath
	sum := sha256.Sum256([]byte(result))
	digest := hex.EncodeToString(sum[:])
	skillViewTrackerMu.Lock()
	defer skillViewTrackerMu.Unlock()
	cache := skillViewTracker[taskID]
	if cache == nil {
		cache = &dedupCache{digests: map[string]string{}}
		skillViewTracker[taskID] = cache
	}
	if d, ok := cache.digests[key]; ok && d == digest {
		file := filePath
		if file == "" {
			file = "SKILL.md"
		}
		return jsonResult(map[string]any{"success": true, "status": "unchanged", "name": resolved, "file": file,
			"dedup": true, "content_returned": false, "message": skillViewDedupMessage})
	}
	if _, ok := cache.digests[key]; ok {
		for i, k := range cache.order {
			if k == key {
				cache.order = append(cache.order[:i], cache.order[i+1:]...)
				break
			}
		}
	}
	cache.digests[key] = digest
	cache.order = append(cache.order, key)
	for len(cache.order) > skillViewDedupCap {
		delete(cache.digests, cache.order[0])
		cache.order = cache.order[1:]
	}
	return ""
}

// Clears the skill view dedup cache of one task, every task when taskID is empty
func ResetSkillViewDedup(taskID string) {
	skillViewTrackerMu.Lock()
	defer skillViewTrackerMu.Unlock()
	if taskID == "" {
		skillViewTracker = map[string]*dedupCache{}
		return
	}
	delete(skillViewTracker, taskID)
}
