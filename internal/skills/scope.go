package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// What a skills operation is done for: the settings and folder of a profile, the folder a session works in, the surface it is shown on
// and the tools it is offered
type Scope struct {
	registry *Registry
	Config   *config.Config
	// Folder the session works in, where the skills of a trusted project are found; none for no project
	Cwd string
	// Surface the session is shown on, such as "tui", which skills.platform_disabled and a skill's session_platforms read; none for any
	Platform string
	// Session the operation is for, which ${ICEBATH_SESSION_ID} becomes and usage records
	Session string
	// Names of the tools offered on the turn and of their toolsets, which a skill's conditions read; nil when not known
	Tools, Toolsets map[string]bool
	// Asks the user for the value of an environment variable the skill needs, the answer kept for the profile; nil where no one can be
	// asked
	Ask func(ctx context.Context, skill string, variable Variable) (string, error)
	// Backend the session's commands run on when it is off this machine, where what a skill needs must be too; none for this machine
	Remote string
	// Variables a command run for a skill gets, such as a shell snippet; the process's own when nil
	Env func() map[string]string
	// Asks the user to allow an action, an error when they don't; nil where no one can be asked
	Approve func(ctx context.Context, action, reason string) error
}

// Scope over the profile of the settings and the folder a session works in, on the surface the environment names when it names one
func (r *Registry) Scope(cfg *config.Config, cwd string) *Scope {
	platform := strings.TrimSpace(os.Getenv("ICEBATH_PLATFORM"))
	if platform == "" {
		platform = strings.TrimSpace(os.Getenv("ICEBATH_SESSION_PLATFORM"))
	}
	return &Scope{registry: r, Config: cfg, Cwd: cwd, Platform: platform}
}

// Precedence tiers of skill folders, lowest wins
const (
	TierProject = iota
	TierLocal
	TierCreate
	TierExternal
)

// Folder names never scanned for skills
var excluded = map[string]bool{
	".git": true, ".github": true, ".hub": true, ".archive": true, ".curator_backups": true, ".locks": true, ".icebath-dist": true,
	".venv": true, "venv": true, "node_modules": true, "site-packages": true, "__pycache__": true, ".tox": true, ".nox": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true, ".restore-backups": true,
}

// Folders of a skill's supporting files, never skills themselves
var supportDirs = []string{"references", "templates", "assets", "scripts"}

// Skills that can never be disabled or deleted, since the system prompt points at them
var Essential = map[string]bool{"icebath-agent": true}

// Project folders holding a repository's own skills
var projectSkillDirs = []string{filepath.Join(".icebath", "skills"), filepath.Join(".agents", "skills")}

// Folder of the profile
func (s *Scope) Home() string {
	return s.Config.ProfileDir(s.Config.Profile())
}

// The profile's own skills folder
func (s *Scope) Dir() string {
	return filepath.Join(s.Home(), "skills")
}

// Display form of a path, the user's home folder as ~
func display(p string) string {
	if home, err := os.UserHomeDir(); err == nil && within(p, home) {
		rel, _ := filepath.Rel(home, p)
		if rel == "." {
			return "~"
		}
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}

// Expands ~ and environment variables in a configured path, a relative one taken from the profile's folder
func (s *Scope) path(entry string) string {
	p := fsx.ExpandHome(s.expandVars(entry))
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Home(), p)
	}
	return resolve(p)
}

// Variables of the profile's environment in a value replaced, those it has not set left as they are
func (s *Scope) expandVars(value string) string {
	return os.Expand(value, func(name string) string {
		if v, ok := s.Config.LookupEnv(name); ok {
			return v
		}
		return "${" + name + "}"
	})
}

// Path with symlinks resolved, the cleaned absolute path when that fails
func resolve(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// Whether child is parent or lies under it
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// Whether the path is a symlink or a junction a recursive delete could leave the tree through
func redirect(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && (info.Mode()&os.ModeSymlink != 0 || runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0)
}

// Configured extra skill folders that exist, without repeats or the profile's own folder
func (s *Scope) External() []string {
	local := resolve(s.Dir())
	var out []string
	for _, entry := range list(s.Config.Get(skillsconfig.ExternalDirs)) {
		p := s.path(entry)
		if p != local && !slices.Contains(out, p) && fsx.IsDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// Configured folder new skills are written to, none when unset or the profile's own folder
func (s *Scope) CreateDir() string {
	raw := strings.TrimSpace(s.Config.GetString(skillsconfig.CreateDir))
	if raw == "" {
		return ""
	}
	if p := s.path(raw); p != resolve(s.Dir()) {
		return p
	}
	return ""
}

// Where new skills are created, as the user reads it
func (s *Scope) CreateDisplay() string {
	if dir := s.CreateDir(); dir != "" {
		return display(dir) + "/"
	}
	return display(s.Dir()) + "/"
}

// One skill folder with its precedence
type root struct {
	tier int
	dir  string
}

// Every skill folder in precedence order: the trusted project's, the profile's, the create folder, the extra ones
func (s *Scope) roots(project bool) []root {
	var roots []root
	if project {
		for _, dir := range s.ProjectDirs() {
			roots = append(roots, root{TierProject, dir})
		}
	}
	roots = append(roots, root{TierLocal, s.Dir()})
	if dir := s.CreateDir(); dir != "" && fsx.IsDir(dir) {
		roots = append(roots, root{TierCreate, dir})
	}
	for _, dir := range s.External() {
		roots = append(roots, root{TierExternal, dir})
	}
	var out []root
	for _, r := range roots {
		if !slices.ContainsFunc(out, func(o root) bool { return o.dir == r.dir }) {
			out = append(out, r)
		}
	}
	return out
}

// Roots a lookup searches: every root, the profile's own only when it exists
func (s *Scope) searched() []root {
	return slices.DeleteFunc(s.roots(true), func(r root) bool { return r.tier == TierLocal && !fsx.Exists(r.dir) })
}

// Skill folders in precedence order, without the project's
func (s *Scope) Dirs() []string {
	var out []string
	for _, r := range s.roots(false) {
		out = append(out, r.dir)
	}
	return out
}

// Nearest folder above the working folder holding .git, none when there is none or it is the user's home folder
func (s *Scope) projectRoot() string {
	if s.Cwd == "" {
		return ""
	}
	home := ""
	if dir, err := os.UserHomeDir(); err == nil {
		home = resolve(dir)
	}
	cur := resolve(s.Cwd)
	for range 64 {
		if fsx.Lexists(filepath.Join(cur, ".git")) {
			if cur == home {
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

// Whether the project folder is one the user trusts with its own skills
func (s *Scope) Trusted(root string) bool {
	root = resolve(root)
	for _, entry := range list(s.Config.Get(skillsconfig.TrustedProjectDirs)) {
		if s.path(entry) == root {
			return true
		}
	}
	return false
}

// Project root of the working folder when project skills are on and its trust is as asked
func (s *Scope) project(trusted bool) string {
	if !s.Config.GetBool(skillsconfig.ProjectDiscovery) {
		return ""
	}
	root := s.projectRoot()
	if root == "" || s.Trusted(root) != trusted {
		return ""
	}
	return root
}

// Skill folders of a project that exist, never the profile's own folder
func (s *Scope) projectDirs(root string) []string {
	local := resolve(s.Dir())
	var out []string
	for _, sub := range projectSkillDirs {
		if dir := filepath.Join(root, sub); fsx.IsDir(dir) && resolve(dir) != local {
			out = append(out, resolve(dir))
		}
	}
	return out
}

// Skill folders of the trusted project the session works in
func (s *Scope) ProjectDirs() []string {
	root := s.project(true)
	if root == "" {
		return nil
	}
	return s.projectDirs(root)
}

// Project the session works in, whether it is trusted, and how many skills it keeps in its own folder; none when project skills are off or
// the folder is in no project
func (s *Scope) Project() (string, bool, int) {
	if !s.Config.GetBool(skillsconfig.ProjectDiscovery) {
		return "", false, 0
	}
	root := s.projectRoot()
	if root == "" {
		return "", false, 0
	}
	count := 0
	for _, dir := range s.projectDirs(root) {
		count += len(skillFiles(dir))
	}
	return root, s.Trusted(root), count
}

// Whether the path lies in an extra skill folder or a trusted project's, which belong to others and are never changed by maintenance
func (s *Scope) ExternalPath(p string) bool {
	p = resolve(p)
	for _, dir := range append(s.External(), s.ProjectDirs()...) {
		if within(p, resolve(dir)) {
			return true
		}
	}
	return false
}

// Disabled skill names: the global list and the surface's, never the essential skills
func (s *Scope) Disabled() map[string]bool {
	out := namesOf(s.Config.Get(skillsconfig.Disabled))
	if s.Platform != "" {
		perPlatform, _ := s.Config.Get(skillsconfig.PlatformDisabled).(map[string]any)
		for name := range namesOf(perPlatform[s.Platform]) {
			out[name] = true
		}
	}
	for name := range Essential {
		delete(out, name)
	}
	return out
}

// Names of a configured list: a list, one name, or a mapping's keys
func namesOf(v any) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case string:
		if name := strings.TrimSpace(t); name != "" {
			out[name] = true
		}
	case []any, []string:
		for _, name := range list(t) {
			out[name] = true
		}
	case map[string]any:
		for name := range t {
			if name = strings.TrimSpace(name); name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// Platform a skill names, as the platform of this machine is spelled
var platformNames = map[string]string{"macos": "darwin", "linux": "linux", "windows": "win32"}

// This machine's platform as skills spell it: linux, darwin, win32, else the name Go knows it by
func platform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Whether a skill declaring the platforms runs on this machine, none meaning all; a name matches this platform's start
func platformMatches(platforms any) bool {
	if !config.Truthy(platforms) {
		return true
	}
	items, ok := platforms.([]any)
	if !ok {
		items = []any{platforms}
	}
	for _, item := range items {
		name := strings.TrimSpace(strings.ToLower(text(item)))
		if mapped, ok := platformNames[name]; ok {
			name = mapped
		}
		if strings.HasPrefix(platform(), name) {
			return true
		}
	}
	return false
}

// Whether a skill runs on this machine's platform
func Supported(fm Frontmatter) bool {
	return platformMatches(fm["platforms"])
}

// Whether any environment a skill declares is the one it runs in, none meaning all, unknown names counting as present
func (s *Scope) environmentMatches(fm Frontmatter) bool {
	declared := list(fm["environments"])
	if len(declared) == 0 {
		return true
	}
	for _, name := range declared {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "kanban":
			if s.Toolsets["kanban"] || os.Getenv("ICEBATH_KANBAN_TASK") != "" || os.Getenv("ICEBATH_KANBAN_BOARD") != "" {
				return true
			}
		case "docker":
			if container() {
				return true
			}
		case "s6":
			if fsx.Exists("/run/s6") || fsx.Exists("/package/admin/s6-overlay") {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// Whether this process runs in a container
func container() bool {
	if fsx.Exists("/.dockerenv") || fsx.Exists("/run/.containerenv") || os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, marker := range []string{"docker", "podman", "/lxc/", "kubepods", "containerd", "crio"} {
			if strings.Contains(string(data), marker) {
				return true
			}
		}
	}
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 5 && fields[4] == "/" {
				for _, marker := range []string{"kubepods", "containerd", "crio"} {
					if strings.Contains(line, marker) {
						return true
					}
				}
			}
		}
	}
	return false
}

// Whether every app a skill requires is known and can be offered
func (s *Scope) appsMatch(fm Frontmatter) bool {
	required := list(fm["requires_apps"])
	if len(required) == 0 {
		return true
	}
	apps, ok := capability[Apps](s.registry)
	if !ok {
		return false
	}
	for _, name := range required {
		if declared, offerable := apps.App(name); !declared || !offerable {
			return false
		}
	}
	return true
}

// Whether a skill can be offered here at all: its platforms, environments and apps
func (s *Scope) compatible(fm Frontmatter) bool {
	return platformMatches(fm["platforms"]) && s.environmentMatches(fm) && s.appsMatch(fm)
}

// Whether a skill's conditions let it show: its surfaces, and the tools and toolsets it requires or stands in for
func (s *Scope) shows(conditions map[string][]string) bool {
	if wanted := conditions["session_platforms"]; len(wanted) > 0 && s.Platform != "" &&
		!slices.ContainsFunc(wanted, func(p string) bool { return strings.EqualFold(strings.TrimSpace(p), s.Platform) }) {
		return false
	}
	if s.Tools == nil && s.Toolsets == nil {
		return true
	}
	for _, toolset := range conditions["fallback_for_toolsets"] {
		if s.Toolsets[toolset] {
			return false
		}
	}
	for _, tool := range conditions["fallback_for_tools"] {
		if s.Tools[tool] {
			return false
		}
	}
	for _, toolset := range conditions["requires_toolsets"] {
		if !s.Toolsets[toolset] {
			return false
		}
	}
	for _, tool := range conditions["requires_tools"] {
		if !s.Tools[tool] {
			return false
		}
	}
	return true
}

// Whether a path has an excluded folder in it, or lies in a support folder of a skill
func excludedPath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if excluded[part] {
			return true
		}
	}
	return supportPath(p)
}

// Whether a path lies under a support folder directly inside a skill
func supportPath(p string) bool {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i := 1; i < len(parts)-1; i++ {
		if !slices.Contains(supportDirs, parts[i]) {
			continue
		}
		dir := filepath.FromSlash(strings.Join(parts[:i], "/"))
		if filepath.IsAbs(p) && !filepath.IsAbs(dir) {
			dir = string(filepath.Separator) + dir
		}
		if fsx.IsFile(filepath.Join(dir, "SKILL.md")) {
			return true
		}
	}
	return false
}

// Sorted SKILL.md paths under a folder, symlinked folders followed, excluded folders and the support folders of skills skipped
func skillFiles(dir string) []string {
	var out []string
	walk(dir, map[string]bool{}, func(folder string, files []string) {
		if slices.Contains(files, "SKILL.md") {
			out = append(out, filepath.Join(folder, "SKILL.md"))
		}
	})
	slices.Sort(out)
	return out
}

// Walks a tree following symlinked folders, each real folder once, skipping excluded folders and the support folders of a skill,
// visiting each folder's files
func walk(dir string, seen map[string]bool, visit func(folder string, files []string)) {
	real := resolve(dir)
	if seen[real] {
		return
	}
	seen[real] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var dirs, files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 && fsx.IsDir(filepath.Join(dir, name)) {
			dirs = append(dirs, name)
			continue
		}
		files = append(files, name)
	}
	slices.Sort(dirs)
	slices.Sort(files)
	skill := slices.Contains(files, "SKILL.md")
	visit(dir, files)
	for _, name := range dirs {
		if excluded[name] || skill && slices.Contains(supportDirs, name) {
			continue
		}
		walk(filepath.Join(dir, name), seen, visit)
	}
}

// Whether every path is the same skill: one file resolved, or the same bytes
func same(paths []string) bool {
	real := map[string]bool{}
	for _, p := range paths {
		real[resolve(p)] = true
	}
	if len(real) == 1 {
		return true
	}
	digests := map[string]bool{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(data)
		digests[hex.EncodeToString(sum[:])] = true
	}
	return len(digests) == 1
}
