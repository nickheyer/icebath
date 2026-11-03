package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Default places components sit inside a plugin
const (
	skillsDir    = "skills"
	commandsDir  = "commands"
	agentsDir    = "agents"
	hooksFile    = "hooks/hooks.json"
	serversFile  = ".mcp.json"
	portableMCP  = "mcp.json"
	stylesDir    = "output-styles"
	monitorsFile = "monitors/monitors.json"
	binDir       = "bin"
)

// Where a plugin was found
const (
	FromMarketplace = "marketplace"
	FromDir         = "inline"
	FromSkillsDir   = "skills-dir"
	FromClaude      = "claude"
	FromCodex       = "codex"
)

// Plugin loaded from a directory with every component it provides
type Plugin struct {
	// Id the person enables it by, entry@marketplace or name@inline
	ID string
	// Manifest name, the namespace of every component
	Name string
	// Marketplace entry name, the install id
	Entry       string
	Marketplace string
	Root        string
	// Persistent data directory kept across updates
	Data     string
	Version  string
	Manifest Manifest
	From     string
	Enabled  bool
	Skills   []*Skill
	Agents   []*Agent
	Styles   []*Style
	Hooks    agent.HookConfig
	Servers  []*Server
	Monitors []Monitor
	// Directory of executables put on the shell path, empty when the plugin has none
	Bin string
	// Values of the user options, by key
	Options map[string]string
	// Options required that hold no value
	Missing []string
	// Problems found loading, the plugin still loaded
	Problems []string
	// The marketplace entry with its own component declarations, applied over the manifest
	entry *Entry
}

// What a load needs beside the directory
type LoadOptions struct {
	ID          string
	Entry       string
	Marketplace string
	Data        string
	From        string
	Options     map[string]string
	// Marketplace entry the plugin was installed from, nil for none
	Entry_ *Entry
	// Project root the components are told about
	ProjectDir string
}

// Reads a plugin from its directory, every component it declares or holds at the default place
func Load(root string, opts LoadOptions) (*Plugin, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !isDir(root) {
		return nil, fmt.Errorf("plugin directory %s does not exist", root)
	}
	m, err := ReadManifest(root)
	if err != nil {
		return nil, err
	}
	entry := opts.Entry_
	if m.Format == FormatBare && entry != nil {
		m = entry.AsManifest(m.Name)
		if opts.Entry != "" {
			m.Name = opts.Entry
		}
	}
	if m.Format == FormatBare && opts.Entry != "" {
		m.Name = opts.Entry
	}
	p := &Plugin{Name: m.Name, Entry: opts.Entry, Marketplace: opts.Marketplace, Root: root, Data: opts.Data, Version: m.Version, Manifest: m, From: opts.From, Options: opts.Options, entry: entry}
	if p.Entry == "" {
		p.Entry = p.Name
	}
	p.ID = opts.ID
	if p.ID == "" {
		origin := opts.Marketplace
		if origin == "" {
			origin = FromDir
		}
		p.ID = p.Entry + "@" + origin
	}
	if p.Data == "" {
		p.Data = filepath.Join(os.TempDir(), "icebath-plugin-data", DataID(p.ID))
	}
	if p.Options == nil {
		p.Options = map[string]string{}
	}
	for key, opt := range m.UserConfig {
		if _, ok := p.Options[key]; !ok {
			if d := opt.DefaultText(); d != "" {
				p.Options[key] = d
			}
		}
		if opt.Required && strings.TrimSpace(p.Options[key]) == "" {
			p.Missing = append(p.Missing, key)
		}
	}
	sort.Strings(p.Missing)
	p.Problems = append(p.Problems, m.Warnings...)
	if entry != nil && m.Format != FormatBare {
		if err := p.applyEntry(entry); err != nil {
			return nil, err
		}
	}
	p.load(opts.ProjectDir)
	return p, nil
}

// Applies the component additions of a marketplace entry over the manifest, refusing a conflict in lax mode
func (p *Plugin) applyEntry(e *Entry) error {
	m := &p.Manifest
	if !e.Strict() && e.hasComponents() {
		return fmt.Errorf("plugin %s has conflicting manifests: both plugin.json and marketplace entry specify components", p.Name)
	}
	if e.DisplayName != "" {
		m.DisplayName = e.DisplayName
	}
	if e.Description != "" {
		m.Description = e.Description
	}
	if e.Author != nil {
		m.Author = *e.Author
	}
	if e.Homepage != "" {
		m.Homepage = e.Homepage
	}
	if e.Repository != "" {
		m.Repository = e.Repository
	}
	if e.License != "" {
		m.License = e.License
	}
	if len(e.Keywords) > 0 {
		m.Keywords = e.Keywords
	}
	if e.DefaultEnabled != nil {
		m.DefaultEnabled = e.DefaultEnabled
	}
	if m.Version == "" {
		m.Version = e.Version
	}
	m.Dependencies = append(m.Dependencies, e.Dependencies...)
	m.Skills = append(m.Skills, e.Skills...)
	if len(e.Commands) > 0 {
		m.Commands = append(m.Commands, e.Commands...)
		m.CommandsSet = true
	}
	if len(e.Agents) > 0 {
		m.Agents = append(m.Agents, e.Agents...)
		m.AgentsSet = true
	}
	if len(e.OutputStyles) > 0 {
		m.Styles = append(m.Styles, e.OutputStyles...)
		m.StylesSet = true
	}
	if len(e.Hooks) > 0 {
		m.HookInline = append(m.HookInline, e.Hooks)
	}
	m.HookFiles = append(m.HookFiles, e.HookPaths...)
	return nil
}

// Variables substituted into the components of the plugin and exported to its processes
func (p *Plugin) Vars(projectDir string) map[string]string {
	vars := map[string]string{
		"CLAUDE_PLUGIN_ROOT": p.Root, "CLAUDE_PLUGIN_DATA": p.Data, "PLUGIN_ROOT": p.Root, "PLUGIN_DATA": p.Data,
		"ICEBATH_PLUGIN_ROOT": p.Root, "ICEBATH_PLUGIN_DATA": p.Data,
	}
	if projectDir != "" {
		vars["CLAUDE_PROJECT_DIR"] = projectDir
		vars["ICEBATH_PROJECT_DIR"] = projectDir
	}
	keys := make([]string, 0, len(p.Options))
	for k := range p.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vars["user_config."+k] = p.Options[k]
		vars["CLAUDE_PLUGIN_OPTION_"+strings.ToUpper(k)] = p.Options[k]
	}
	return vars
}

// Variables for skill and agent text, sensitive options shown as a placeholder
func (p *Plugin) textVars(projectDir string) map[string]string {
	vars := p.Vars(projectDir)
	for key, opt := range p.Manifest.UserConfig {
		if opt.Sensitive {
			vars["user_config."+key] = "[sensitive value " + key + "]"
			delete(vars, "CLAUDE_PLUGIN_OPTION_"+strings.ToUpper(key))
		}
	}
	return vars
}

// Reads every component of the plugin
func (p *Plugin) load(projectDir string) {
	p.loadSkills()
	p.loadCommands()
	p.loadAgents()
	p.loadHooks(projectDir)
	p.loadServers()
	p.loadStyles()
	p.loadMonitors()
	if isDir(filepath.Join(p.Root, binDir)) {
		p.Bin = filepath.Join(p.Root, binDir)
	}
	for _, s := range p.Skills {
		s.Plugin, s.Origin = p, OriginPlugin
		s.Name = pluginName(p.Name, SafeName(s.Short))
		s.Short = strings.TrimPrefix(s.Name, p.Name+":")
	}
	for _, a := range p.Agents {
		a.Plugin, a.Origin = p, OriginPlugin
		a.Name = p.Name + ":" + a.Short
		a.PermissionMode, a.Hooks, a.MCPServers, a.InitialPrompt = "", nil, nil, ""
	}
	for _, s := range p.Styles {
		s.Plugin, s.Origin = p, OriginPlugin
		s.Name = p.Name + ":" + s.Short
	}
}

func (p *Plugin) problem(format string, args ...any) {
	p.Problems = append(p.Problems, fmt.Sprintf(format, args...))
}

// Directories a manifest path names inside the root, each checked for existence and containment
func (p *Plugin) resolve(kind string, paths []string) []string {
	var out []string
	for _, raw := range paths {
		if !relativePath(raw) {
			p.problem("%s path %q must start with ./", kind, raw)
			continue
		}
		full, err := inside(p.Root, raw)
		if err != nil {
			p.problem("%s %v", kind, err)
			continue
		}
		out = append(out, full)
	}
	return out
}

// Skills from the default directory, the manifest directories and a root SKILL.md
func (p *Plugin) loadSkills() {
	dirs := []string{filepath.Join(p.Root, skillsDir)}
	dirs = append(dirs, p.resolve("skills", p.Manifest.Skills)...)
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if exists(filepath.Join(dir, "SKILL.md")) {
			s, err := ReadSkill(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				p.problem("skills: %v", err)
				continue
			}
			if dir == p.Root {
				s.Short = p.Name
			}
			p.Skills = append(p.Skills, s)
			continue
		}
		list, err := readSkillDir(dir)
		if err != nil {
			p.problem("skills: %v", err)
			continue
		}
		p.Skills = append(p.Skills, list...)
	}
	if len(p.Skills) == 0 && len(p.Manifest.Skills) == 0 && exists(filepath.Join(p.Root, "SKILL.md")) {
		s, err := ReadSkill(filepath.Join(p.Root, "SKILL.md"))
		if err != nil {
			p.problem("skills: %v", err)
			return
		}
		p.Skills = append(p.Skills, s)
	}
}

// Commands from the default directory, or the manifest paths and inline commands when set
func (p *Plugin) loadCommands() {
	m := p.Manifest
	if !m.CommandsSet {
		list, err := readCommandDir(filepath.Join(p.Root, commandsDir))
		if err != nil {
			p.problem("commands: %v", err)
		}
		p.Skills = append(p.Skills, list...)
		return
	}
	if isDir(filepath.Join(p.Root, commandsDir)) {
		p.problem("default commands/ folder is ignored because the manifest sets \"commands\"")
	}
	for _, path := range p.resolve("commands", m.Commands) {
		if isDir(path) {
			list, err := readCommandDir(path)
			if err != nil {
				p.problem("commands: %v", err)
			}
			p.Skills = append(p.Skills, list...)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			p.problem("commands: %v", err)
			continue
		}
		p.Skills = append(p.Skills, ParseCommand(string(data), path, filepath.Dir(path)))
	}
	keys := make([]string, 0, len(m.CommandMap))
	for k := range m.CommandMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c := m.CommandMap[key]
		text := c.Content
		file := filepath.Join(p.Root, ".claude-plugin", "plugin.json")
		if c.Source != "" {
			paths := p.resolve("commands", []string{c.Source})
			if len(paths) == 0 {
				continue
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				p.problem("commands: %v", err)
				continue
			}
			text, file = string(data), paths[0]
		}
		s := ParseSkill(text, file)
		s.Legacy, s.Short = true, key
		if c.Description != "" {
			s.Description = c.Description
		}
		if c.ArgumentHint != "" {
			s.ArgumentHint = c.ArgumentHint
		}
		if c.Model != "" {
			s.Model = c.Model
		}
		if len(c.AllowedTools) > 0 {
			s.AllowedTools = c.AllowedTools
		}
		p.Skills = append(p.Skills, s)
	}
}

// Agents from the default directory with subfolders in their names, or the manifest files when set
func (p *Plugin) loadAgents() {
	m := p.Manifest
	if !m.AgentsSet {
		list, err := readAgentDir(filepath.Join(p.Root, agentsDir), true, true)
		if err != nil {
			p.problem("agents: %v", err)
		}
		p.Agents = append(p.Agents, list...)
		return
	}
	if isDir(filepath.Join(p.Root, agentsDir)) {
		p.problem("default agents/ folder is ignored because the manifest sets \"agents\"")
	}
	for _, path := range p.resolve("agents", m.Agents) {
		if isDir(path) {
			p.problem("agents: %s is a directory, entries must be .md files", path)
			continue
		}
		a, err := readAgentFile(path)
		if err != nil {
			p.problem("agents: %v", err)
			continue
		}
		p.Agents = append(p.Agents, a)
	}
}

// Hooks from the default file, then every manifest file and inline map, labeled with the plugin variables
func (p *Plugin) loadHooks(projectDir string) {
	vars := p.Vars(projectDir)
	var configs []agent.HookConfig
	if data, err := os.ReadFile(filepath.Join(p.Root, hooksFile)); err == nil {
		cfg, err := agent.ParseHooks(data, true)
		if err != nil {
			p.problem("%s: %v", hooksFile, err)
		} else {
			configs = append(configs, cfg)
		}
	} else if !os.IsNotExist(err) {
		p.problem("%s: %v", hooksFile, err)
	}
	for _, path := range p.resolve("hooks", p.Manifest.HookFiles) {
		data, err := os.ReadFile(path)
		if err != nil {
			p.problem("hooks: %v", err)
			continue
		}
		cfg, err := agent.ParseHooks(data, true)
		if err != nil {
			p.problem("%s: %v", path, err)
			continue
		}
		configs = append(configs, cfg)
	}
	for _, raw := range p.Manifest.HookInline {
		cfg, err := agent.ParseHooks(raw, false)
		if err != nil {
			p.problem("hooks: %v", err)
			continue
		}
		configs = append(configs, cfg)
	}
	if p.entry != nil && len(p.entry.Hooks) > 0 && p.entry.Strict() {
		entryCfg, err := agent.ParseHooks(p.entry.Hooks, false)
		if err != nil {
			p.problem("marketplace hooks: %v", err)
		} else {
			merged := agent.HookConfig{}
			for _, c := range configs {
				for ev, groups := range c {
					if _, replaced := entryCfg[ev]; replaced {
						continue
					}
					merged[ev] = append(merged[ev], groups...)
				}
			}
			for ev, groups := range entryCfg {
				merged[ev] = append(merged[ev], groups...)
			}
			configs = []agent.HookConfig{merged}
		}
	}
	if len(configs) == 0 {
		return
	}
	for i, c := range configs {
		for _, groups := range c {
			for _, g := range groups {
				for _, h := range g.Hooks {
					if len(h.Args) == 0 && strings.Contains(h.Command, "${user_config.") {
						p.problem("hook %q references ${user_config.*} in a shell form command, read CLAUDE_PLUGIN_OPTION_<KEY> instead", h.Command)
					}
				}
			}
		}
		configs[i] = c.Labeled(agent.SourcePlugin, p.Name, vars)
	}
	p.Hooks = agent.MergeHooks(configs...)
}

// Servers from the default file, the portable file, then every manifest file and inline map, later names replacing earlier
func (p *Plugin) loadServers() {
	var all []*Server
	add := func(list []*Server, err error) {
		if err != nil {
			p.problem("%v", err)
		}
		all = append(all, list...)
	}
	add(readServersFile(filepath.Join(p.Root, serversFile)))
	if path := filepath.Join(p.Root, portableMCP); exists(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			p.problem("%v", err)
		} else {
			var doc struct {
				Schema string `json:"$schema"`
			}
			json.Unmarshal(data, &doc)
			if doc.Schema != "" && doc.Schema != PortableMCPSchema {
				p.problem("%s: schema %s does not match %s, servers disabled", portableMCP, doc.Schema, PortableMCPSchema)
			} else {
				list, err := parseServers(data)
				if err != nil {
					p.problem("%s: %v", path, err)
				}
				all = append(all, list...)
			}
		}
	}
	for _, path := range p.resolve("mcpServers", p.Manifest.ServerFiles) {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".mcpb", ".dxt":
			list, err := readBundle(path, filepath.Join(p.Root, ".mcpb-cache"))
			add(list, err)
		default:
			add(readServersFile(path))
		}
	}
	for _, raw := range p.Manifest.ServerInline {
		list, err := parseServers(raw)
		add(list, err)
	}
	byName := map[string]int{}
	var out []*Server
	for _, s := range all {
		s.Plugin = p
		if i, ok := byName[s.Name]; ok {
			out[i] = s
			continue
		}
		byName[s.Name] = len(out)
		out = append(out, s)
	}
	p.Servers = out
}

// Styles from the default directory, or the manifest paths when set
func (p *Plugin) loadStyles() {
	m := p.Manifest
	if !m.StylesSet {
		list, err := readStyles(filepath.Join(p.Root, stylesDir))
		if err != nil {
			p.problem("output-styles: %v", err)
		}
		p.Styles = append(p.Styles, list...)
		return
	}
	if isDir(filepath.Join(p.Root, stylesDir)) {
		p.problem("default output-styles/ folder is ignored because the manifest sets \"outputStyles\"")
	}
	for _, path := range p.resolve("outputStyles", m.Styles) {
		list, err := readStyles(path)
		if err != nil {
			p.problem("output-styles: %v", err)
		}
		p.Styles = append(p.Styles, list...)
	}
}

// Monitors from the manifest, else the default file
func (p *Plugin) loadMonitors() {
	if len(p.Manifest.Monitors) > 0 {
		p.Monitors = p.Manifest.Monitors
		return
	}
	data, err := os.ReadFile(filepath.Join(p.Root, monitorsFile))
	if err != nil {
		if !os.IsNotExist(err) {
			p.problem("%s: %v", monitorsFile, err)
		}
		return
	}
	var list []Monitor
	if err := json.Unmarshal(data, &list); err != nil {
		p.problem("%s: %v", monitorsFile, err)
		return
	}
	for _, m := range list {
		if m.Name == "" || m.Command == "" {
			p.problem("%s: each monitor needs a name and a command", monitorsFile)
			return
		}
	}
	p.Monitors = list
}

// Name shown for the plugin, the display name when it has one
func (p *Plugin) Title() string {
	if p.Manifest.DisplayName != "" {
		return p.Manifest.DisplayName
	}
	if p.Manifest.Interface.DisplayName != "" {
		return p.Manifest.Interface.DisplayName
	}
	return p.Name
}

// Reports whether the plugin starts enabled when the person has not said
func (p *Plugin) DefaultEnabled() bool {
	return p.Manifest.DefaultEnabled == nil || *p.Manifest.DefaultEnabled
}

// Servers of the plugin with their variables substituted, named plugin_<plugin>_<server> so tools carry the plugin
func (p *Plugin) ServersFor(projectDir string) []*Server {
	vars := p.Vars(projectDir)
	out := make([]*Server, 0, len(p.Servers))
	for _, s := range p.Servers {
		sub := s.Substituted(vars)
		sub.Name = "plugin_" + p.Name + "_" + s.Name
		if sub.Transport == TransportStdio && sub.Dir == "" {
			sub.Dir = p.Root
		}
		if sub.Env == nil {
			sub.Env = map[string]string{}
		}
		for _, k := range []string{"CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA", "PLUGIN_ROOT", "PLUGIN_DATA"} {
			sub.Env[k] = vars[k]
		}
		out = append(out, sub)
	}
	return out
}

// Error naming every problem of the plugin, nil when it has none
func (p *Plugin) Err() error {
	if len(p.Problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(p.Problems, "; "))
}
