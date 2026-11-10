package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Directory profile distribution installs stage into before an atomic swap
const DistSwapDir = ".icebath-dist"

// Directory names never scanned for skills
var ExcludedSkillDirs = map[string]bool{
	".git": true, ".github": true, ".hub": true, ".archive": true, ".curator_backups": true, ".locks": true,
	DistSwapDir: true, ".venv": true, "venv": true, "node_modules": true, "site-packages": true,
	"__pycache__": true, ".tox": true, ".nox": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
}

// Support directories inside a skill package loaded through skill_view file_path
var SkillSupportDirs = map[string]bool{"references": true, "templates": true, "assets": true, "scripts": true}

// Skills that can never be disabled because the system prompt points at them
var EssentialSkills = map[string]bool{"icebath-agent": true}

// Precedence tiers, lowest wins
const (
	TierProject = iota
	TierLocal
	TierCreateDir
	TierExternal
)

// Leading words of every same tier ambiguity refusal
const AmbiguousSkillPrefix = "Ambiguous skill name "

// Platform names a skill may declare mapped to the Python sys.platform prefix they match
var platformMap = map[string]string{"macos": "darwin", "linux": "linux", "windows": "win32"}

// Reports whether a path has an excluded directory component or sits in a support package
func isExcludedSkillPath(path string, root string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if ExcludedSkillDirs[part] {
			return true
		}
	}
	return isSkillSupportPath(path, root)
}

// Reports whether a path lies under a support directory that sits directly inside a skill root
func isSkillSupportPath(path string, root string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	base := ""
	if root != "" && !filepath.IsAbs(path) {
		base = root
	}
	for idx := 1; idx < len(parts)-1; idx++ {
		if !SkillSupportDirs[parts[idx]] {
			continue
		}
		dir := filepath.Join(parts[:idx]...)
		if base != "" {
			dir = filepath.Join(base, dir)
		} else if filepath.IsAbs(path) && !filepath.IsAbs(dir) {
			dir = string(filepath.Separator) + dir
		}
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
			return true
		}
	}
	return false
}

// Reports whether a platforms declaration includes sys.platform of this host, falsy meaning all
func skillMatchesPlatformList(platforms any) bool {
	if !pyrepr.Truthy(platforms) {
		return true
	}
	items, isList := platforms.([]any)
	if !isList {
		items = []any{platforms}
	}
	current := hostinfo.OSFamily()
	for _, p := range items {
		norm := pystr.Strip(pystr.Lower(pyrepr.Str(p)))
		mapped, ok := platformMap[norm]
		if !ok {
			mapped = norm
		}
		if strings.HasPrefix(current, mapped) {
			return true
		}
	}
	return false
}

// Reports whether a frontmatter platforms gate admits the running OS
func skillMatchesPlatform(fm map[string]any) bool {
	return skillMatchesPlatformList(fm["platforms"])
}

var (
	envDetectMu    sync.Mutex
	envDetectCache = map[string]bool{}
)

// Reports whether this process runs inside a container
func isContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, m := range []string{"docker", "podman", "/lxc/", "kubepods", "containerd", "crio"} {
			if strings.Contains(string(data), m) {
				return true
			}
		}
	}
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) >= 5 && f[4] == "/" {
				for _, m := range []string{"kubepods", "containerd", "crio"} {
					if strings.Contains(line, m) {
						return true
					}
				}
			}
		}
	}
	return false
}

// Reports whether the kanban environment is active for this process
func (s *Scope) detectKanban() bool {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" || os.Getenv("ICEBATH_KANBAN_BOARD") != "" {
		return true
	}
	if cfg := s.tree(); cfg != nil {
		for _, ts := range config.AsStrings(cfg["toolsets"]) {
			if ts == "kanban" {
				return true
			}
		}
	}
	return false
}

// Reports whether a named runtime environment is active, unknown names counting as active
func (s *Scope) detectEnvironment(env string) bool {
	if env != "kanban" {
		envDetectMu.Lock()
		cached, ok := envDetectCache[env]
		envDetectMu.Unlock()
		if ok {
			return cached
		}
	}
	var result bool
	switch env {
	case "kanban":
		result = s.detectKanban()
	case "docker":
		result = isContainer()
	case "s6":
		_, e1 := os.Stat("/run/s6")
		_, e2 := os.Stat("/package/admin/s6-overlay")
		result = e1 == nil || e2 == nil
	default:
		result = true
	}
	envDetectMu.Lock()
	envDetectCache[env] = result
	envDetectMu.Unlock()
	return result
}

// Reports whether any declared environments tag is active, absent meaning all
func (s *Scope) skillMatchesEnvironment(fm map[string]any) bool {
	tags := fmStringList(fm["environments"])
	if len(tags) == 0 {
		return true
	}
	for _, t := range tags {
		if s.detectEnvironment(strings.ToLower(strings.TrimSpace(t))) {
			return true
		}
	}
	return false
}

// AppAvailability answers whether a declared app has a registered declaration this host can offer
var AppAvailability func(name string) (declared bool, offerable bool)

// Reports whether every requires_apps name resolves to an offerable declaration
func skillMatchesApps(fm map[string]any) bool {
	names := fmStringList(fm["requires_apps"])
	if len(names) == 0 {
		return true
	}
	if AppAvailability == nil {
		return false
	}
	for _, n := range names {
		declared, offerable := AppAvailability(strings.TrimSpace(n))
		if !declared || !offerable {
			return false
		}
	}
	return true
}

// Normalizes a config value that may hold a JSON array string into a string list
func parseConfigStringList(v any) []string {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(pystr.Strip(t), "[") {
			items, ok, err := pyast.LiteralStrList(t)
			if err != nil {
				panic(err)
			}
			if ok {
				return items
			}
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, pyrepr.Str(item))
		}
		return out
	case []string:
		return t
	}
	return nil
}

// Stripped non empty names of a config list value as a set
func normalizeStringSet(v any) map[string]bool {
	out := map[string]bool{}
	for _, n := range parseConfigStringList(v) {
		if s := strings.TrimSpace(n); s != "" {
			out[s] = true
		}
	}
	return out
}

// Disabled skill names from config, the global list joined with the platform list minus essentials
func (s *Scope) DisabledSkillNames(platform string) map[string]bool {
	cfg := s.skillsCfg()
	if cfg == nil {
		return map[string]bool{}
	}
	if platform == "" {
		platform = s.Platform
	}
	disabled := normalizeStringSet(cfg["disabled"])
	if platform != "" {
		if pd := config.AsMap(cfg["platform_disabled"]); pd != nil {
			if v, ok := pd[platform]; ok && v != nil {
				for n := range normalizeStringSet(v) {
					disabled[n] = true
				}
			}
		}
	}
	for n := range EssentialSkills {
		delete(disabled, n)
	}
	return disabled
}

// Reports whether any of the names is disabled for the platform
func (s *Scope) isSkillDisabled(platform string, names ...string) bool {
	disabled := s.DisabledSkillNames(platform)
	for _, n := range names {
		if n != "" && disabled[n] {
			return true
		}
	}
	return false
}

// Path with symlinks resolved, the cleaned absolute path when resolution fails
func resolvePath(p string) string {
	if r, err := pypath.Realpath(p, pypath.Lax); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// Reports whether child equals or lies under parent lexically
func isRelativeTo(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Reports whether a directory exists at the path
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Reports whether a regular file exists at the path
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Reports whether anything exists at the path without following a final symlink
func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Validated deduplicated external skill dirs from config, existing dirs only and never the local dir
func (s *Scope) ExternalSkillsDirs() []string {
	key, keyed := "", false
	if s.live {
		ctx := context.Background()
		configPath := home.ConfigPath(s.Home)
		exists, err := config.ConfigExists(ctx, configPath)
		if err != nil {
			panic(err)
		}
		if !exists {
			return nil
		}
		key, keyed = configCacheKey(ctx, configPath)
		if keyed {
			externalDirsCache.mu.Lock()
			cached, ok := externalDirsCache.m[key]
			externalDirsCache.mu.Unlock()
			if ok {
				return append([]string(nil), cached...)
			}
		}
	}
	cfg := s.skillsCfg()
	if cfg == nil {
		return nil
	}
	local := resolvePath(s.SkillsDir())
	var out []string
	for _, entry := range fmStringList(cfg["external_dirs"]) {
		p := resolvePath(s.homeRelative(expandPath(entry)))
		if p == local || slices.Contains(out, p) {
			continue
		}
		if isDir(p) {
			out = append(out, p)
		} else {
			s.logger().Debug("External skills dir does not exist, skipping", "dir", p)
		}
	}
	if keyed {
		externalDirsCache.mu.Lock()
		externalDirsCache.m[key] = append([]string(nil), out...)
		externalDirsCache.mu.Unlock()
	}
	return out
}

// Configured skills.create_dir, empty when unset or equal to the local skills dir
func (s *Scope) SkillCreateDir() string {
	raw := strings.TrimSpace(fmString(s.skillsCfgGet("create_dir")))
	if raw == "" {
		return ""
	}
	p := resolvePath(s.homeRelative(expandPath(raw)))
	if p == resolvePath(s.SkillsDir()) {
		return ""
	}
	return p
}

// User facing path where new skills are created
func (s *Scope) DisplaySkillCreateDir() string {
	createDir := s.SkillCreateDir()
	if createDir == "" {
		return s.displayHome() + "/skills/"
	}
	if u, err := os.UserHomeDir(); err == nil && isRelativeTo(createDir, u) {
		rel, _ := filepath.Rel(u, createDir)
		return "~/" + filepath.ToSlash(rel) + "/"
	}
	return filepath.ToSlash(createDir) + "/"
}

// One skill root with its precedence tier
type skillRoot struct {
	tier int
	dir  string
}

// Every skill root in precedence order, the one ordering every listing and lookup shares
func (s *Scope) skillSearchRoots(local string, includeProject bool) []skillRoot {
	var roots []skillRoot
	if includeProject {
		for _, d := range s.ProjectSkillsDirs() {
			roots = append(roots, skillRoot{TierProject, d})
		}
	}
	if local == "" {
		local = s.SkillsDir()
	}
	roots = append(roots, skillRoot{TierLocal, local})
	if cd := s.SkillCreateDir(); cd != "" && isDir(cd) {
		roots = append(roots, skillRoot{TierCreateDir, cd})
	}
	for _, d := range s.ExternalSkillsDirs() {
		roots = append(roots, skillRoot{TierExternal, d})
	}
	seen := map[string]bool{}
	out := roots[:0]
	for _, r := range roots {
		if seen[r.dir] {
			continue
		}
		seen[r.dir] = true
		out = append(out, r)
	}
	return out
}

// Skill dirs in precedence order without trusted project dirs
func (s *Scope) AllSkillsDirs() []string {
	var out []string
	for _, r := range s.skillSearchRoots("", false) {
		out = append(out, r.dir)
	}
	return out
}

// Reports whether every path is the same skill as one resolved file or byte identical content
func provablySameSkill(skillMDs []string) bool {
	real := map[string]bool{}
	for _, p := range skillMDs {
		real[resolvePath(p)] = true
	}
	if len(real) == 1 {
		return true
	}
	digests := map[string]bool{}
	for _, p := range skillMDs {
		data, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(data)
		digests[hex.EncodeToString(sum[:])] = true
	}
	return len(digests) == 1
}

// Same root order of identical copies, a real SKILL.md before a flat markdown and shallower first
func skillCandidateRank(skillMD, root string) [2]int {
	flat := 0
	if filepath.Base(skillMD) != "SKILL.md" {
		flat = 1
	}
	rel, err := filepath.Rel(root, skillMD)
	depth := 0
	if err == nil {
		depth = len(strings.Split(filepath.ToSlash(rel), "/"))
	}
	return [2]int{flat, depth}
}

// Compares two ranks lexicographically
func rankLess(a, b [2]int) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

// One identifier candidate with its tier, root, same root rank, and SKILL.md path
type skillCandidate struct {
	tier    int
	root    string
	rank    [2]int
	skillMD string
}

// Winner index among candidates and the top tier contender indexes, the winner negative when ambiguous
func pickSkillCandidate(cands []skillCandidate) (int, []int) {
	top := cands[0].tier
	for _, c := range cands[1:] {
		if c.tier < top {
			top = c.tier
		}
	}
	var contenders []int
	for i, c := range cands {
		if c.tier == top {
			contenders = append(contenders, i)
		}
	}
	if len(contenders) > 1 {
		sameRoot := true
		var paths []string
		for _, i := range contenders {
			if cands[i].root != cands[contenders[0]].root {
				sameRoot = false
			}
			paths = append(paths, cands[i].skillMD)
		}
		if sameRoot && provablySameSkill(paths) {
			ranked := append([]int(nil), contenders...)
			sort.SliceStable(ranked, func(a, b int) bool { return rankLess(cands[ranked[a]].rank, cands[ranked[b]].rank) })
			if cands[ranked[0]].rank != cands[ranked[1]].rank {
				return ranked[0], contenders
			}
		}
	}
	if len(contenders) == 1 {
		return contenders[0], contenders
	}
	return -1, contenders
}

// One scanned skill annotated with what skill_view resolves for it
type catalogEntry struct {
	Name         string
	Description  string
	Category     string
	Tier         int
	Root         string
	Path         string
	Visible      bool
	RelativePath string
	Status       string
	LoadName     string
	// Carried index metadata when the entry comes from a prompt snapshot
	SkillName    string
	Conditions   map[string][]string
	Platforms    []string
	RequiresApps []string
	Rel          string
}

var (
	shadowCheckedMu sync.Mutex
	shadowChecked   = map[string]bool{}
)

// Reports whether a resolved entry is disabled by its declared name or its load name
func isDisabledEntry(e catalogEntry, disabled map[string]bool) bool {
	return disabled[e.Name] || (e.LoadName != "" && disabled[e.LoadName])
}

// Annotates scanned skills with status and load name using the same precedence skill_view applies
func (s *Scope) resolveSkillCatalog(entries []catalogEntry) []catalogEntry {
	out := make([]catalogEntry, len(entries))
	copy(out, entries)
	owners := map[string][]int{}
	for i := range out {
		skillDir := filepath.Dir(out[i].Path)
		rel, err := filepath.Rel(out[i].Root, skillDir)
		if err != nil {
			rel = skillDir
		}
		out[i].RelativePath = filepath.ToSlash(rel)
		aliases := map[string]bool{out[i].Name: true, filepath.Base(skillDir): true, out[i].RelativePath: true}
		for alias := range aliases {
			owners[alias] = append(owners[alias], i)
		}
	}
	winner := map[string]int{}
	for alias, idxs := range owners {
		cands := make([]skillCandidate, 0, len(idxs))
		for _, j := range idxs {
			cands = append(cands, skillCandidate{out[j].Tier, out[j].Root, skillCandidateRank(out[j].Path, out[j].Root), out[j].Path})
		}
		won, _ := pickSkillCandidate(cands)
		if won < 0 {
			winner[alias] = -1
		} else {
			winner[alias] = idxs[won]
		}
	}
	for i := range out {
		name, rel := out[i].Name, out[i].RelativePath
		var higher []int
		for _, j := range owners[name] {
			if out[j].Tier < out[i].Tier {
				higher = append(higher, j)
			}
		}
		if len(higher) > 0 || (winner[name] != -1 && winner[name] != i) {
			out[i].Status, out[i].LoadName = "shadowed", ""
			if len(higher) > 0 {
				keyParts := []string{out[i].Path}
				for _, j := range higher {
					keyParts = append(keyParts, out[j].Path)
				}
				sort.Strings(keyParts[1:])
				key := strings.Join(keyParts, "\x00")
				shadowCheckedMu.Lock()
				seen := shadowChecked[key]
				shadowChecked[key] = true
				shadowCheckedMu.Unlock()
				if !seen {
					same := false
					for _, j := range higher {
						if provablySameSkill([]string{out[i].Path, out[j].Path}) {
							same = true
							break
						}
					}
					if !same {
						s.logger().Warn("skill is shadowed by a higher precedence copy (project > local > create_dir > external_dirs)", "skill", name, "path", out[i].Path)
					}
				}
			}
		} else if winner[name] == i {
			out[i].Status, out[i].LoadName = "unique", name
		} else {
			out[i].Status = "ambiguous"
			if winner[rel] == i {
				out[i].LoadName = rel
			} else {
				out[i].LoadName = ""
			}
		}
	}
	return out
}

// Project local skill subdirectories relative to a repository root
var projectSkillsSubdirs = []string{filepath.Join(".icebath", "skills"), filepath.Join(".agents", "skills")}

const projectRootMaxDepth = 64

// Nearest ancestor of start holding a .git entry, empty when none or when it is the user home
func findProjectRoot(start string) string {
	if start == "" {
		return ""
	}
	cur := resolvePath(start)
	userHome := ""
	if u, err := os.UserHomeDir(); err == nil {
		userHome = resolvePath(u)
	}
	for i := 0; i < projectRootMaxDepth; i++ {
		if lexists(filepath.Join(cur, ".git")) {
			if cur == userHome {
				return ""
			}
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}

// Resolved trusted project roots from skills.trusted_project_dirs
func (s *Scope) trustedProjectDirs() map[string]bool {
	out := map[string]bool{}
	for _, entry := range fmStringList(s.skillsCfgGet("trusted_project_dirs")) {
		out[resolvePath(expandPath(entry))] = true
	}
	return out
}

// Reports whether a project root is listed in skills.trusted_project_dirs
func (s *Scope) IsProjectRootTrusted(root string) bool {
	return s.trustedProjectDirs()[resolvePath(root)]
}

// Existing skill dirs under a project root, excluding the profile skills dir itself
func (s *Scope) candidateProjectSkillsDirs(root string) []string {
	local := resolvePath(s.SkillsDir())
	var out []string
	for _, sub := range projectSkillsSubdirs {
		cand := filepath.Join(root, sub)
		if isDir(cand) && resolvePath(cand) != local {
			out = append(out, resolvePath(cand))
		}
	}
	return out
}

// Project root of the cwd when discovery is on and its trust state matches
func (s *Scope) currentProjectRoot(trusted bool) string {
	if v, ok := s.skillsCfgGet("project_discovery").(bool); ok && !v {
		return ""
	}
	root := s.FindProjectRoot()
	if root == "" || s.IsProjectRootTrusted(root) != trusted {
		return ""
	}
	return root
}

// Trusted project local skill dirs for the current cwd
func (s *Scope) ProjectSkillsDirs() []string {
	root := s.currentProjectRoot(true)
	if root == "" {
		return nil
	}
	return s.candidateProjectSkillsDirs(root)
}

// Untrusted project root of the cwd and its skill count when it holds skills
func (s *Scope) UntrustedProjectSkillsRoot() (string, int) {
	root := s.currentProjectRoot(false)
	if root == "" {
		return "", 0
	}
	count := 0
	for _, d := range s.candidateProjectSkillsDirs(root) {
		count += len(iterSkillIndexFiles(d, "SKILL.md"))
	}
	if count == 0 {
		return "", 0
	}
	return root, count
}

var (
	quarantineMu    sync.Mutex
	quarantineCache = map[string]bool{}
)

const projectScanSource = "project-local"

// Scanner project skill quarantine runs, replaceable so tests can make it fail
var quarantineScan = ScanSkillCached

// Reports whether a project skill scanned dangerous, failing closed on any scan problem
func (s *Scope) isQuarantinedProjectSkill(skillMD string) bool {
	skillDir := filepath.Dir(skillMD)
	key := resolvePath(skillDir)
	quarantineMu.Lock()
	cached, ok := quarantineCache[key]
	quarantineMu.Unlock()
	if ok {
		return cached
	}
	cacheDir := filepath.Join(s.Home, "cache", "project_skill_scans")
	result, _, err := quarantineScan(skillDir, projectScanSource, "", cacheDir)
	quarantined := true
	if err != nil {
		s.logger().Warn("project skill scan failed, quarantining", "skill", skillDir, "err", err)
	} else {
		quarantined = result.Verdict == "dangerous"
		if quarantined {
			s.logger().Warn("project skill quarantined", "skill", skillDir, "summary", result.Summary)
		}
	}
	quarantineMu.Lock()
	quarantineCache[key] = quarantined
	quarantineMu.Unlock()
	return quarantined
}

// Non quarantined SKILL.md files under a trusted project dir
func (s *Scope) iterProjectSkillFiles(projectDir string) []string {
	var out []string
	for _, p := range iterSkillIndexFiles(projectDir, "SKILL.md") {
		if !s.isQuarantinedProjectSkill(p) {
			out = append(out, p)
		}
	}
	return out
}

// NormalizeSkillLookupName turns a trusted absolute skill path into the relative name skill_view accepts
func (s *Scope) NormalizeSkillLookupName(identifier string) string {
	return s.normalizeSkillLookupName(identifier)
}

// Translates a trusted absolute skill path to the relative form skill_view accepts
func (s *Scope) normalizeSkillLookupName(identifier string) string {
	raw := strings.TrimSpace(identifier)
	if raw == "" {
		return raw
	}
	p := expandPath(raw)
	if !filepath.IsAbs(p) {
		return strings.TrimLeft(raw, "/")
	}
	primary := s.SkillsDir()
	roots := append([]string{primary}, s.ProjectSkillsDirs()...)
	roots = append(roots, s.ExternalSkillsDirs()...)
	for _, root := range roots {
		if isRelativeTo(p, root) {
			rel, _ := filepath.Rel(root, p)
			return rel
		}
	}
	if rel, err := filepath.Rel(resolvePath(primary), resolvePath(p)); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	s.logger().Debug("skill identifier is an absolute path outside trusted roots", "identifier", raw)
	return raw
}

// Reports whether a path lives under an external or trusted project skills dir
func (s *Scope) isExternalSkillPath(path string) bool {
	cand := resolvePath(expandPath(path))
	roots := append(s.ExternalSkillsDirs(), s.ProjectSkillsDirs()...)
	for _, r := range roots {
		if isRelativeTo(cand, resolvePath(r)) {
			return true
		}
	}
	return false
}

// Reports whether lifecycle maintenance must treat the path as an externally owned read only skill
func (s *Scope) IsExternalSkillPath(path string) bool { return s.isExternalSkillPath(path) }

// Conditional activation keys read from the icebath metadata
var conditionKeys = []string{"fallback_for_toolsets", "requires_toolsets", "fallback_for_tools", "requires_tools", "session_platforms"}

// Conditional activation lists of a frontmatter, absent keys empty
func extractSkillConditions(fm map[string]any) map[string][]string {
	meta := icebathMeta(fm)
	out := map[string][]string{}
	for _, k := range conditionKeys {
		out[k] = fmStringList(meta[k])
	}
	return out
}

// One declared skill config variable
type SkillConfigVar struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Default     any    `json:"default,omitempty"`
	Prompt      string `json:"prompt"`
	Skill       string `json:"skill,omitempty"`
}

// Declared metadata config variables of a frontmatter, entries without key or description dropped
func extractSkillConfigVars(fm map[string]any) []SkillConfigVar {
	raw := icebathMeta(fm)["config"]
	var items []any
	switch t := raw.(type) {
	case map[string]any:
		items = []any{t}
	case []any:
		items = t
	default:
		return nil
	}
	var out []SkillConfigVar
	seen := map[string]bool{}
	for _, item := range items {
		m := fmMap(item)
		if m == nil {
			continue
		}
		key := strings.TrimSpace(fmString(m["key"]))
		desc := strings.TrimSpace(fmString(m["description"]))
		if key == "" || seen[key] || desc == "" {
			continue
		}
		seen[key] = true
		v := SkillConfigVar{Key: key, Description: desc, Prompt: desc}
		if m["default"] != nil {
			v.Default = m["default"]
		}
		if p := strings.TrimSpace(fmString(m["prompt"])); p != "" {
			v.Prompt = p
		}
		out = append(out, v)
	}
	return out
}

// Config variable declarations across every enabled platform compatible skill, deduplicated by key
func (s *Scope) DiscoverAllSkillConfigVars() []SkillConfigVar {
	var out []SkillConfigVar
	seen := map[string]bool{}
	disabled := s.DisabledSkillNames("")
	for _, dir := range s.AllSkillsDirs() {
		if !isDir(dir) {
			continue
		}
		for _, skillFile := range iterSkillIndexFiles(dir, "SKILL.md") {
			data, err := os.ReadFile(skillFile)
			if err != nil {
				continue
			}
			fm, _ := ParseFrontmatter(string(data))
			name := fmString(fm["name"])
			if name == "" {
				name = filepath.Base(filepath.Dir(skillFile))
			}
			if disabled[name] || !skillMatchesPlatform(fm) {
				continue
			}
			for _, v := range extractSkillConfigVars(fm) {
				if !seen[v.Key] {
					seen[v.Key] = true
					v.Skill = name
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// Config prefix under which skill config values live
const SkillConfigPrefix = "skills.config"

var homeVarRe = regexp.MustCompile(`\$(?:\{HOME\}|HOME)`)

// Expands a tilde and HOME references against the tool subprocess home
func (s *Scope) expandSkillConfigPath(value string) string {
	subHome := tools.SubprocessHome(s.registrationEnv())
	if subHome != "" {
		if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
			value = subHome + value[1:]
		}
		value = replaceHomeVars(value, subHome)
	}
	return expandPath(value)
}

// Replaces each HOME reference ending at a separator or the end of the value with the subprocess home
func replaceHomeVars(value, subHome string) string {
	var b strings.Builder
	last := 0
	for _, m := range homeVarRe.FindAllStringIndex(value, -1) {
		if m[1] < len(value) && value[m[1]] != '/' && value[m[1]] != '\\' {
			continue
		}
		b.WriteString(value[last:m[0]])
		b.WriteString(subHome)
		last = m[1]
	}
	b.WriteString(value[last:])
	return b.String()
}

// Walks a nested mapping along a dotted key, nil when any part is missing
func resolveDotpath(cfg map[string]any, dotted string) any {
	var cur any = cfg
	for _, part := range strings.Split(dotted, ".") {
		m := config.AsMap(cur)
		if m == nil {
			return nil
		}
		next, ok := m[part]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// Project root of the effective working directory, empty when none
func (s *Scope) FindProjectRoot() string { return findProjectRoot(s.agentCwd()) }

// Working directory project discovery starts from, the session cwd then TERMINAL_CWD then the scope cwd
func (s *Scope) agentCwd() string {
	cwd, err := tools.ResolveAgentCwd(s.registrationEnv())
	if err != nil {
		if config.IsOSError(err) {
			return ""
		}
		panic(err)
	}
	return cwd
}

// Current values of declared config variables, declared defaults filling gaps and paths expanded
func (s *Scope) ResolveSkillConfigValues(vars []SkillConfigVar) map[string]any {
	raw := s.rawConfig()
	out := map[string]any{}
	for _, v := range vars {
		var value any
		value = resolveDotpath(raw, SkillConfigPrefix+"."+v.Key)
		if value == nil || (isString(value) && strings.TrimSpace(value.(string)) == "") {
			value = v.Default
			if value == nil {
				value = ""
			}
		}
		if str, ok := value.(string); ok && (strings.Contains(str, "~") || strings.Contains(str, "$")) {
			value = s.expandSkillConfigPath(str)
		}
		out[v.Key] = value
	}
	return out
}

// Reports whether a value is a Go string
func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// Description width the system prompt index shows per row
const SkillPromptDescLimit = 60

var (
	lastWhitespaceRe = regexp.MustCompile(`[` + pySpaceClass + `][^` + pySpaceClass + `]*\n?\z`)
	clipTrailRe      = regexp.MustCompile(`[` + pySpaceClass + `.,;:\-–—(/]+\n?\z`)
)

// Normalized description of a frontmatter for comparison and truncation
func normalizeSkillDescription(fm map[string]any) string {
	raw := fm["description"]
	if !pyrepr.Truthy(raw) {
		return ""
	}
	return strings.Trim(pystr.Strip(pyrepr.Str(raw)), "'\"")
}

// Description fitted to the index budget and cut at a word boundary
func ClipSkillDescription(desc string) string {
	runes := []rune(desc)
	if len(runes) <= SkillPromptDescLimit {
		return desc
	}
	head := string(runes[:SkillPromptDescLimit-2])
	loc := lastWhitespaceRe.FindStringIndex(head)
	if loc == nil || loc[0] == 0 {
		return string(runes[:SkillPromptDescLimit-3]) + "..."
	}
	return clipTrailRe.ReplaceAllString(head[:loc[0]], "") + "..."
}

// System prompt length description of a frontmatter
func extractSkillDescription(fm map[string]any) string {
	return ClipSkillDescription(normalizeSkillDescription(fm))
}

// Reports whether the description will be clipped in the prompt index
func isSkillDescriptionTruncatedForPrompt(fm map[string]any) bool {
	return len([]rune(normalizeSkillDescription(fm))) > SkillPromptDescLimit
}

// Sorted paths named filename under a skills dir, pruning excluded dirs and support dirs of skills
func iterSkillIndexFiles(skillsDir string, filename string) []string {
	var matches []string
	walkFollowingLinks(skillsDir, func(dir string, names []string, hasSkillMD bool) []string {
		kept := names[:0]
		for _, d := range names {
			if ExcludedSkillDirs[d] || (hasSkillMD && SkillSupportDirs[d]) {
				continue
			}
			kept = append(kept, d)
		}
		return kept
	}, func(dir string, files []string) {
		for _, f := range files {
			if f == filename {
				matches = append(matches, filepath.Join(dir, f))
			}
		}
	})
	sort.Strings(matches)
	return matches
}

// Directory listing the skill tree walk reads, os.scandir in the hermes walk, swapped by tests
var walkReadDir = os.ReadDir

// Walks a tree through every symlinked directory path like os.walk with followlinks, pruning children through the callback
func walkFollowingLinks(root string, prune func(dir string, dirs []string, hasSkillMD bool) []string, visit func(dir string, files []string)) {
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := walkReadDir(dir)
		if err != nil {
			return
		}
		var dirs, files []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				dirs = append(dirs, name)
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
					dirs = append(dirs, name)
					continue
				}
			}
			files = append(files, name)
		}
		hasSkillMD := false
		for _, f := range files {
			if f == "SKILL.md" {
				hasSkillMD = true
			}
		}
		sort.Strings(dirs)
		sort.Strings(files)
		if prune != nil {
			dirs = prune(dir, dirs, hasSkillMD)
		}
		visit(dir, files)
		for _, d := range dirs {
			walk(filepath.Join(dir, d))
		}
	}
	walk(root)
}

// Every file under a directory as paths relative with forward slashes, sorted
func rglobFiles(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

var namespaceRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Splits a namespace qualified skill name, the namespace empty without a colon
func parseQualifiedName(name string) (string, string, bool) {
	ns, bare, ok := strings.Cut(name, ":")
	if !ok {
		return "", name, false
	}
	return ns, bare, true
}

// Reports whether a namespace is well formed
func isValidNamespace(candidate string) bool {
	return candidate != "" && namespaceRe.MatchString(candidate)
}

// Reads a text file with a BOM stripped and invalid bytes replaced
func readSkillText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return decodeText(data), nil
}

// Decodes bytes as UTF-8 with a BOM stripped and invalid sequences replaced
func decodeText(data []byte) string {
	text := strings.ToValidUTF8(string(data), "�")
	return strings.TrimPrefix(text, "\uFEFF")
}

// Exported form for pkg/memory, same answer as the package reader
func ReadSkillName(skillMD string, fallback string) string { return readSkillName(skillMD, fallback) }

// Frontmatter name of a SKILL.md read from its first 4000 characters, else the fallback
func readSkillName(skillMD string, fallback string) string {
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return fallback
	}
	return skillNameFromText(decodeText(data), fallback)
}

// Frontmatter name in the first 4000 characters of a text, else the fallback
func skillNameFromText(text string, fallback string) string {
	runes := []rune(text)
	if len(runes) > 4000 {
		text = string(runes[:4000])
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	start := -1
	for i, l := range lines {
		if l == "---" {
			start = i
			break
		}
	}
	if start < 0 {
		return fallback
	}
	block := lines[start+1:]
	for i, l := range block {
		if l == "---" {
			block = block[:i]
			break
		}
	}
	for _, l := range block {
		if strings.HasPrefix(l, "name:") {
			v := strings.Trim(strings.TrimSpace(l[len("name:"):]), "\"'")
			if v != "" {
				return v
			}
		}
	}
	return fallback
}
