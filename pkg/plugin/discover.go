package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Folders a harness keeps its loose components under, tried in this order inside a home or project directory
var componentDirs = []string{".icebath", ".claude", ".agents", ".codex", ".hermes"}

// Directory holding managed components on this machine
const managedDir = "/etc/claude-code"

// Where the library looks for plugins and loose components
type Sources struct {
	// Configuration directory of this harness, holding user skills, agents and the plugin store
	ConfigDir string
	// Project root and the working directory under it
	Root string
	Dir  string
	// Directories added beside the project
	Added []string
	// Plugin directories loaded in place, each a plugin of its own
	PluginDirs []string
	// Enablement by plugin id, the plugin default when unset
	Enabled map[string]bool
	// Option values of plugins by id and key
	Options map[string]map[string]string
	// Reads the installs of Claude Code and Codex too
	Foreign bool
	// Plugins forced on by a managed policy
	Managed map[string]bool
	// Skills hidden or narrowed by name, on, name-only, user-invocable-only or off
	Overrides map[string]string
}

// Everything discovered, resolved so each name points at one component
type Library struct {
	Plugins []*Plugin
	Skills  []*Skill
	Agents  []*Agent
	Styles  []*Style
	Servers []*Server
	// Hooks of every enabled plugin, labeled with their plugin
	Hooks agent.HookConfig
	// Monitors of every enabled plugin with the plugin they belong to
	Monitors []PluginMonitor
	// Directories of executables enabled plugins add to the shell path
	Bins []string
	// Agent a plugin asks the main session to run as, empty for none
	MainAgent string
	// Style a plugin forces, empty for none
	ForcedStyle string
	// Problems found that did not stop the load
	Problems []string
	byName   map[string]*Skill
	agents   map[string]*Agent
	styles   map[string]*Style
}

// Monitor with the plugin that declared it
type PluginMonitor struct {
	Monitor
	Plugin *Plugin
}

// Reads every source into a library
func Discover(src Sources) (*Library, error) {
	lib := &Library{byName: map[string]*Skill{}, agents: map[string]*Agent{}, styles: map[string]*Style{}}
	if err := lib.loadPlugins(src); err != nil {
		return nil, err
	}
	lib.loadLoose(src)
	lib.resolve(src)
	return lib, nil
}

func (l *Library) problem(format string, args ...any) {
	l.Problems = append(l.Problems, fmt.Sprintf(format, args...))
}

// Loads plugins from the store, the foreign stores, the plugin directories and the skill directory plugins
func (l *Library) loadPlugins(src Sources) error {
	seen := map[string]*Plugin{}
	add := func(p *Plugin, rank int) {
		if on, set := src.Managed[p.ID]; set && on {
			p.Enabled = true
		}
		if have, ok := seen[p.Name]; ok {
			if rankOf(have, src) <= rank {
				l.problem("plugin %s from %s is shadowed by the one from %s", p.ID, p.From, have.ID)
				return
			}
			l.problem("plugin %s from %s is shadowed by the one from %s", have.ID, have.From, p.ID)
			for i, q := range l.Plugins {
				if q == have {
					l.Plugins = append(l.Plugins[:i], l.Plugins[i+1:]...)
					break
				}
			}
		}
		seen[p.Name] = p
		l.Plugins = append(l.Plugins, p)
	}
	for _, dir := range src.PluginDirs {
		p, err := Load(dir, LoadOptions{From: FromDir, ProjectDir: src.Root, Options: src.Options[filepath.Base(dir)+"@"+FromDir]})
		if err != nil {
			l.problem("plugin directory %s: %v", dir, err)
			continue
		}
		p.Enabled = true
		if on, set := src.Enabled[p.ID]; set {
			p.Enabled = on
		}
		add(p, 2)
	}
	if src.ConfigDir != "" {
		store := NewStore(src.ConfigDir)
		plugins, errs, err := store.Load(src.Enabled, src.Options, src.Root)
		if err != nil {
			return err
		}
		for id, err := range errs {
			l.problem("plugin %s: %v", id, err)
		}
		for _, p := range plugins {
			add(p, 3)
		}
	}
	if src.Foreign {
		for _, read := range []func(string) ([]Foreign, error){ClaudeInstalls, CodexInstalls} {
			list, err := read(src.Root)
			if err != nil {
				l.problem("%v", err)
				continue
			}
			for _, f := range list {
				if _, ok := seen[f.Entry]; ok {
					continue
				}
				p, err := Load(f.Root, LoadOptions{ID: f.ID, Entry: f.Entry, Marketplace: f.Marketplace, Data: f.Data, From: f.From, Options: src.Options[f.ID], ProjectDir: src.Root})
				if err != nil {
					l.problem("plugin %s from %s: %v", f.ID, f.From, err)
					continue
				}
				p.Version, p.Enabled = f.Version, f.Enabled
				if on, set := src.Enabled[f.ID]; set {
					p.Enabled = on
				}
				add(p, 3)
			}
		}
	}
	for _, dir := range skillDirs(src) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			root := filepath.Join(dir, e.Name())
			if !exists(filepath.Join(root, ClaudeManifest)) {
				continue
			}
			p, err := Load(root, LoadOptions{ID: e.Name() + "@" + FromSkillsDir, Entry: e.Name(), Marketplace: FromSkillsDir, From: FromSkillsDir, ProjectDir: src.Root, Options: src.Options[e.Name()+"@"+FromSkillsDir]})
			if err != nil {
				l.problem("skills directory plugin %s: %v", root, err)
				continue
			}
			p.Enabled = true
			if on, set := src.Enabled[p.ID]; set {
				p.Enabled = on
			}
			add(p, 4)
		}
	}
	l.enableDependencies()
	sort.SliceStable(l.Plugins, func(i, j int) bool { return l.Plugins[i].ID < l.Plugins[j].ID })
	return nil
}

// Rank of a plugin by where it came from, lower winning a name clash
func rankOf(p *Plugin, src Sources) int {
	if on, set := src.Managed[p.ID]; set && on {
		return 1
	}
	switch p.From {
	case FromDir:
		return 2
	case FromMarketplace, FromClaude, FromCodex:
		return 3
	}
	return 4
}

// Turns on every plugin an enabled plugin depends on
func (l *Library) enableDependencies() {
	byEntry := map[string]*Plugin{}
	for _, p := range l.Plugins {
		byEntry[p.Entry+"@"+p.Marketplace] = p
		byEntry[p.Entry] = p
	}
	changed := true
	for changed {
		changed = false
		for _, p := range l.Plugins {
			if !p.Enabled {
				continue
			}
			for _, d := range p.Manifest.Dependencies {
				key := d.Name
				if d.Marketplace != "" {
					key = d.Name + "@" + d.Marketplace
				} else if p.Marketplace != "" {
					key = d.Name + "@" + p.Marketplace
				}
				dep, ok := byEntry[key]
				if !ok {
					dep, ok = byEntry[d.Name]
				}
				if !ok {
					l.problem("plugin %s depends on %s, which is not installed", p.ID, key)
					continue
				}
				if !dep.Enabled {
					dep.Enabled = true
					changed = true
				}
			}
		}
	}
}

// Skill directories of the user and the project, the places a skills directory plugin may sit
func skillDirs(src Sources) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", skillsDir))
	}
	if src.ConfigDir != "" {
		out = append(out, filepath.Join(src.ConfigDir, skillsDir))
	}
	if src.Root != "" {
		out = append(out, filepath.Join(src.Root, ".claude", skillsDir), filepath.Join(src.Root, ".icebath", skillsDir))
	}
	return out
}

// One directory of loose components with its origin
type looseDir struct {
	path   string
	origin string
	// Directory the components are discovered under, for nested and added naming
	root string
}

// Loose component directories in precedence order, managed, user, project walk, added
func looseDirs(src Sources) []looseDir {
	var out []looseDir
	if isDir(filepath.Join(managedDir, ".claude")) {
		out = append(out, looseDir{path: filepath.Join(managedDir, ".claude"), origin: OriginManaged})
	}
	if src.ConfigDir != "" {
		out = append(out, looseDir{path: src.ConfigDir, origin: OriginUser})
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{".claude", ".agents", ".codex", ".hermes"} {
			if isDir(filepath.Join(home, d)) {
				out = append(out, looseDir{path: filepath.Join(home, d), origin: OriginUser})
			}
		}
	}
	for i, dir := range lineage(src.Root, src.Dir) {
		origin := OriginProject
		if i > 0 {
			origin = OriginNested
		}
		for _, d := range componentDirs {
			if isDir(filepath.Join(dir, d)) {
				out = append(out, looseDir{path: filepath.Join(dir, d), origin: origin, root: dir})
			}
		}
	}
	for _, added := range src.Added {
		for _, d := range componentDirs {
			if isDir(filepath.Join(added, d)) {
				out = append(out, looseDir{path: filepath.Join(added, d), origin: OriginAdded, root: added})
			}
		}
	}
	return out
}

// Directories from root down to dir inclusive, root alone when dir is not under it
func lineage(root, dir string) []string {
	if root == "" {
		root = dir
	}
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	out := []string{root}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return out
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

// Loads skills, commands, agents and styles from every loose directory
func (l *Library) loadLoose(src Sources) {
	for _, d := range looseDirs(src) {
		skills, err := readSkillTree(filepath.Join(d.path, skillsDir))
		if err != nil {
			l.problem("%s: %v", filepath.Join(d.path, skillsDir), err)
		}
		for _, s := range skills {
			if exists(filepath.Join(s.Dir, ClaudeManifest)) {
				continue
			}
			s.Origin, s.Root = d.origin, d.root
			if RefusedName(s.Short) {
				l.problem("skill %s has a refused name", s.File)
				continue
			}
			if strings.HasPrefix(strings.ToLower(s.Short), "anthropic-skills") {
				l.problem("skill %s uses the reserved anthropic-skills name", s.File)
				continue
			}
			l.Skills = append(l.Skills, s)
		}
		commands, err := readCommandDir(filepath.Join(d.path, commandsDir))
		if err != nil {
			l.problem("%s: %v", filepath.Join(d.path, commandsDir), err)
		}
		for _, s := range commands {
			s.Origin, s.Root = d.origin, d.root
			l.Skills = append(l.Skills, s)
		}
		agents, err := readAgentDir(filepath.Join(d.path, agentsDir), false, false)
		if err != nil {
			l.problem("%s: %v", filepath.Join(d.path, agentsDir), err)
		}
		for _, a := range agents {
			a.Origin, a.Name = d.origin, a.Short
			l.Agents = append(l.Agents, a)
		}
		styles, err := readStyles(filepath.Join(d.path, stylesDir))
		if err != nil {
			l.problem("%s: %v", filepath.Join(d.path, stylesDir), err)
		}
		for _, s := range styles {
			s.Origin, s.Name = d.origin, s.Short
			l.Styles = append(l.Styles, s)
		}
	}
}

// Reads skills under a directory, descending one level into category folders that hold no SKILL.md of their own
func readSkillTree(dir string) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Skill
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "synced") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !isDir(path) {
			continue
		}
		if exists(filepath.Join(path, "SKILL.md")) {
			s, err := ReadSkill(filepath.Join(path, "SKILL.md"))
			if err != nil {
				return nil, err
			}
			out = append(out, s)
			continue
		}
		list, err := readSkillDir(path)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// Settles the name of every component, plugin ones namespaced and clashing loose ones qualified by directory
func (l *Library) resolve(src Sources) {
	var skills []*Skill
	for _, p := range l.Plugins {
		if !p.Enabled {
			continue
		}
		for _, s := range p.Skills {
			if !s.Supported(runtime.GOOS) {
				continue
			}
			skills = append(skills, s)
		}
		for _, a := range p.Agents {
			l.Agents = append(l.Agents, a)
		}
		for _, s := range p.Styles {
			l.Styles = append(l.Styles, s)
			if s.Force && l.ForcedStyle == "" {
				l.ForcedStyle = s.Name
			}
		}
		l.Servers = append(l.Servers, p.ServersFor(src.Root)...)
		if p.Bin != "" {
			l.Bins = append(l.Bins, p.Bin)
		}
		for _, m := range p.Monitors {
			l.Monitors = append(l.Monitors, PluginMonitor{Monitor: m, Plugin: p})
		}
		if p.Manifest.Agent != "" {
			l.MainAgent = p.Name + ":" + strings.TrimPrefix(p.Manifest.Agent, p.Name+":")
		}
		for _, prob := range p.Problems {
			l.problem("plugin %s: %s", p.ID, prob)
		}
		if len(p.Missing) > 0 {
			l.problem("plugin %s needs values for %s, run icebath plugin configure %s", p.ID, strings.Join(p.Missing, ", "), p.ID)
		}
	}
	l.Hooks = l.pluginHooks()
	loose := l.Skills
	l.Skills = nil
	rank := map[string]int{OriginManaged: 0, OriginUser: 1, OriginProject: 2, OriginNested: 3, OriginAdded: 4}
	sort.SliceStable(loose, func(i, j int) bool {
		if rank[loose[i].Origin] != rank[loose[j].Origin] {
			return rank[loose[i].Origin] < rank[loose[j].Origin]
		}
		return !loose[i].Legacy && loose[j].Legacy
	})
	taken := map[string]*Skill{}
	addedCount := map[string]int{}
	for _, s := range loose {
		if !s.Supported(runtime.GOOS) {
			continue
		}
		name := s.Short
		switch s.Origin {
		case OriginNested:
			if _, clash := taken[name]; clash {
				rel, err := filepath.Rel(src.Root, s.Root)
				if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
					rel = filepath.Base(s.Root)
				}
				name = filepath.ToSlash(rel) + ":" + s.Short
			}
		case OriginAdded:
			if _, clash := taken[name]; clash {
				folder := filepath.Base(s.Root)
				addedCount[folder]++
				if addedCount[folder] > 1 {
					folder = filepath.Base(filepath.Dir(s.Root)) + "/" + folder
				}
				name = folder + ":" + s.Short
			}
		}
		if have, clash := taken[name]; clash {
			if have.Legacy && !s.Legacy {
				taken[name] = s
				s.Name = name
				continue
			}
			continue
		}
		s.Name = name
		taken[name] = s
	}
	for _, s := range taken {
		l.Skills = append(l.Skills, s)
	}
	for _, s := range skills {
		if _, clash := taken[s.Short]; !clash {
			s.Aliases = append(s.Aliases, s.Short)
			taken[s.Short] = s
		}
		l.Skills = append(l.Skills, s)
	}
	sort.SliceStable(l.Skills, func(i, j int) bool { return l.Skills[i].Name < l.Skills[j].Name })
	for _, s := range l.Skills {
		l.byName[s.Name] = s
		for _, a := range s.Aliases {
			if _, ok := l.byName[a]; !ok {
				l.byName[a] = s
			}
		}
	}
	if len(src.Overrides) > 0 {
		kept := l.Skills[:0]
		for _, s := range l.Skills {
			state := src.Overrides[s.Name]
			if state == "" {
				state = src.Overrides[s.Short]
			}
			if s.Plugin != nil || state == "" || state == "on" {
				kept = append(kept, s)
				continue
			}
			switch state {
			case "name-only":
				s.Description, s.WhenToUse = "", ""
			case "user-invocable-only":
				s.DisableModelInvocation = true
			case "off":
				delete(l.byName, s.Name)
				for _, a := range s.Aliases {
					delete(l.byName, a)
				}
				continue
			}
			kept = append(kept, s)
		}
		l.Skills = kept
	}
	l.resolveAgents(src)
	byStyle := map[string]bool{}
	kept := l.Styles[:0]
	for _, s := range l.Styles {
		if byStyle[s.Name] {
			continue
		}
		byStyle[s.Name] = true
		kept = append(kept, s)
		l.styles[s.Name] = s
	}
	l.Styles = kept
}

// Settles agent names, managed over project over user over plugin, the closest project one winning
func (l *Library) resolveAgents(src Sources) {
	rank := map[string]int{OriginManaged: 0, OriginAdded: 1, OriginNested: 2, OriginProject: 3, OriginUser: 4, OriginPlugin: 5}
	sort.SliceStable(l.Agents, func(i, j int) bool { return rank[l.Agents[i].Origin] < rank[l.Agents[j].Origin] })
	kept := l.Agents[:0]
	for _, a := range l.Agents {
		if _, taken := l.agents[a.Name]; taken {
			continue
		}
		l.agents[a.Name] = a
		kept = append(kept, a)
	}
	l.Agents = kept
	sort.SliceStable(l.Agents, func(i, j int) bool { return l.Agents[i].Name < l.Agents[j].Name })
}

// Hooks of every enabled plugin joined, one plugin per manifest name registering them
func (l *Library) pluginHooks() agent.HookConfig {
	var configs []agent.HookConfig
	for _, p := range l.Plugins {
		if p.Enabled && !p.Hooks.Empty() {
			configs = append(configs, p.Hooks)
		}
	}
	return agent.MergeHooks(configs...)
}

// Skill by any name it answers to
func (l *Library) Skill(name string) (*Skill, bool) {
	s, ok := l.byName[name]
	if !ok {
		s, ok = l.byName[strings.ToLower(name)]
	}
	return s, ok
}

// Agent by name
func (l *Library) Agent(name string) (*Agent, bool) {
	a, ok := l.agents[name]
	return a, ok
}

// Style by name
func (l *Library) Style(name string) (*Style, bool) {
	s, ok := l.styles[name]
	return s, ok
}

// Plugin by id or entry name
func (l *Library) Plugin(id string) (*Plugin, bool) {
	for _, p := range l.Plugins {
		if p.ID == id || p.Entry == id || p.Name == id {
			return p, true
		}
	}
	return nil, false
}

// Skills with the given path globs that match a file, relative to the working directory
func (l *Library) SkillsForPath(dir, path string) []*Skill {
	var out []*Skill
	for _, s := range l.Skills {
		if len(s.Paths) > 0 && matchesAny(s.Paths, dir, path) {
			out = append(out, s)
		}
	}
	return out
}

// Skills the model may load on its own, in name order
func (l *Library) ModelSkills() []*Skill {
	var out []*Skill
	for _, s := range l.Skills {
		if s.ModelInvocable() {
			out = append(out, s)
		}
	}
	return out
}

// Skills a person may run by name, in name order
func (l *Library) UserSkills() []*Skill {
	var out []*Skill
	for _, s := range l.Skills {
		if s.UserInvocable {
			out = append(out, s)
		}
	}
	return out
}
