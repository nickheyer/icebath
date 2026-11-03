package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// File under the project state directory counting skill invocations, so the listing keeps the used ones described
const usageFile = "skill-usage.json"

// Plugins, skills, agents, styles, instructions and the state the app keeps for them
type library struct {
	mu    sync.RWMutex
	lib   *plugin.Library
	store *plugin.Store
	// Instruction files as loaded, always applying ones and path scoped ones
	instructions []plugin.Instruction
	// Directories added beside the project
	added []string
	// Hooks of enabled plugins, labeled
	pluginHooks agent.HookConfig
	// Hooks skills registered, by session id
	sessionHooks map[string][]agent.HookConfig
	// Invocations of each skill by name
	usage map[string]int
	// Files loaded per session, instruction paths and skill names auto loaded by path
	loaded map[string]map[string]bool
	// Text loaded into each session by skill name, the digest of the last content
	content map[string]map[string]string
	// Context waiting for the next run of a session
	pending map[string][]protocol.Part
	// Activations waiting for the next run of a session and the one in force per run
	activations map[string]*activation
}

// Reads every plugin and loose component, the instruction files and the usage counts
func (a *App) loadLibrary() error {
	cfg := a.Config
	configDir, err := profiles.ConfigDir()
	if err != nil {
		return err
	}
	a.lib.store = plugin.NewStore(configDir)
	if err := a.registerConfigMarketplaces(); err != nil {
		return err
	}
	options, err := a.lib.store.OptionsWithSecrets(cfg.Plugins.Config)
	if err != nil {
		return err
	}
	src := plugin.Sources{ConfigDir: configDir, Root: a.Root, Dir: a.Dir, Added: a.lib.added, PluginDirs: cfg.Plugins.Dirs, Enabled: cfg.Plugins.Enabled, Options: options, Foreign: cfg.Plugins.ReadsForeign(), Overrides: cfg.Plugins.SkillOverrides}
	lib, err := plugin.Discover(src)
	if err != nil {
		return err
	}
	instructions := plugin.LoadInstructions(configDir, a.Root, a.Dir)
	for _, dir := range a.lib.added {
		instructions = append(instructions, plugin.LoadInstructions("", dir, dir)...)
	}
	a.lib.mu.Lock()
	a.lib.lib = lib
	a.lib.instructions = instructions
	a.lib.pluginHooks = lib.Hooks
	if a.lib.sessionHooks == nil {
		a.lib.sessionHooks = map[string][]agent.HookConfig{}
		a.lib.loaded = map[string]map[string]bool{}
		a.lib.content = map[string]map[string]string{}
		a.lib.pending = map[string][]protocol.Part{}
		a.lib.activations = map[string]*activation{}
	}
	if a.lib.usage == nil {
		a.lib.usage = a.readUsage()
	}
	a.lib.mu.Unlock()
	a.kindMu.Lock()
	kept := a.kinds[:0]
	for _, k := range a.kinds {
		if !strings.HasPrefix(k.Source, "plugin ") && !strings.HasSuffix(k.Source, ".md") {
			kept = append(kept, k)
		}
	}
	a.kinds = kept
	for _, ag := range lib.Agents {
		k := kindOfAgent(ag)
		if err := k.Validate(); err != nil {
			lib.Problems = append(lib.Problems, err.Error())
			continue
		}
		if i := indexKind(a.kinds, k.Name); i >= 0 {
			if a.kinds[i].Source == "builtin" {
				a.kinds[i] = k
			}
			continue
		}
		a.kinds = append(a.kinds, k)
	}
	sort.Slice(a.kinds, func(i, j int) bool { return a.kinds[i].Name < a.kinds[j].Name })
	a.kindMu.Unlock()
	for _, p := range lib.Problems {
		a.send(NoticeMsg{Level: Warn, Text: p})
	}
	return nil
}

func indexKind(kinds []Kind, name string) int {
	for i, k := range kinds {
		if k.Name == name {
			return i
		}
	}
	return -1
}

// Registers the marketplaces the configuration names that the store does not know yet
func (a *App) registerConfigMarketplaces() error {
	if len(a.Config.Plugins.Marketplaces) == 0 {
		return nil
	}
	known, err := a.lib.store.Known()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(a.Config.Plugins.Marketplaces))
	for name := range a.Config.Plugins.Marketplaces {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := known[name]; ok {
			continue
		}
		src := a.Config.Plugins.Marketplaces[name]
		if src.Kind == plugin.MarketSettings && src.Name == "" {
			src.Name = name
		}
		if _, err := a.lib.store.AddMarketplace(context.Background(), src, name); err != nil {
			a.send(NoticeMsg{Level: Warn, Text: "marketplace " + name + ": " + err.Error()})
		}
	}
	return nil
}

// Library as last loaded
func (a *App) Library() *plugin.Library {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	return a.lib.lib
}

// Plugin store of the app
func (a *App) PluginStore() *plugin.Store {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	return a.lib.store
}

// Instruction files as last loaded
func (a *App) Instructions() []plugin.Instruction {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	return append([]plugin.Instruction(nil), a.lib.instructions...)
}

// Reads every plugin, skill, agent, style and instruction file again, connecting the servers that changed
func (a *App) ReloadPlugins(ctx context.Context) error {
	before := a.Library()
	if err := a.loadLibrary(); err != nil {
		return err
	}
	after := a.Library()
	have := map[string]bool{}
	for _, s := range after.Servers {
		have[s.Name] = true
	}
	if before != nil {
		for _, s := range before.Servers {
			if !have[s.Name] {
				a.MCP.Remove(s.Name)
			}
		}
	}
	a.connectPluginServers(ctx, after)
	a.applyBins(after)
	return nil
}

// Adds every server of the enabled plugins the manager does not hold yet
func (a *App) connectPluginServers(ctx context.Context, lib *plugin.Library) {
	known := map[string]bool{}
	for _, st := range a.MCP.Status() {
		known[st.Name] = true
	}
	for _, s := range lib.Servers {
		if known[s.Name] {
			continue
		}
		cfg, err := serverConfig(s)
		if err != nil {
			a.send(NoticeMsg{Level: Warn, Text: err.Error()})
			continue
		}
		if err := a.MCP.Add(ctx, cfg); err != nil {
			a.send(NoticeMsg{Level: Warn, Text: "plugin server " + s.Name + ": " + err.Error()})
		}
	}
}

// Configuration of a plugin server as the manager takes it, missing user values refused
func serverConfig(s *plugin.Server) (ServerConfig, error) {
	cfg := ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, Dir: s.Dir, URL: s.URL, Headers: s.Headers}
	if s.HeadersHelper != "" {
		headers, err := helperHeadersOf(s)
		if err != nil {
			return cfg, err
		}
		if cfg.Headers == nil {
			cfg.Headers = map[string]string{}
		}
		for k, v := range headers {
			cfg.Headers[k] = v
		}
	}
	for key, opt := range s.UserConfig {
		if opt.Required {
			if s.Plugin == nil || strings.TrimSpace(s.Plugin.Options[key]) == "" {
				return cfg, fmt.Errorf("bundled MCP server %q was not started: it needs configuration, %s", s.Name, key)
			}
		}
	}
	return cfg, nil
}

// Runs the headers helper of a server with the plugin variables and reads the JSON object it prints
func helperHeadersOf(s *plugin.Server) (map[string]string, error) {
	env := os.Environ()
	if s.Plugin != nil {
		env = append(env, "CLAUDE_PLUGIN_ROOT="+s.Plugin.Root, "CLAUDE_PLUGIN_DATA="+s.Plugin.Data)
	}
	env = append(env, "CLAUDE_CODE_MCP_SERVER_NAME="+s.Name, "CLAUDE_CODE_MCP_SERVER_URL="+s.URL)
	out, err := runHelper(s.HeadersHelper, env)
	if err != nil {
		return nil, fmt.Errorf("server %s headersHelper: %w", s.Name, err)
	}
	var headers map[string]string
	if err := json.Unmarshal(out, &headers); err != nil {
		return nil, fmt.Errorf("server %s headersHelper must print one JSON object: %w", s.Name, err)
	}
	return headers, nil
}

// Puts the bin directories of enabled plugins on the shell path after the user's own entries
func (a *App) applyBins(lib *plugin.Library) {
	if len(lib.Bins) == 0 {
		return
	}
	path := os.Getenv("PATH")
	parts := []string{path}
	for _, b := range lib.Bins {
		if !strings.Contains(path, b) {
			parts = append(parts, b)
		}
	}
	a.Shell.SetEnv("PATH", strings.Join(parts, string(os.PathListSeparator)))
}

// Adds a directory whose skills, commands, agents and instructions load beside the project, firing DirectoryAdded
func (a *App) AddDir(ctx context.Context, dir, source string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	a.lib.mu.Lock()
	for _, have := range a.lib.added {
		if have == dir {
			a.lib.mu.Unlock()
			return nil
		}
	}
	a.lib.added = append(a.lib.added, dir)
	a.lib.mu.Unlock()
	if err := a.ReloadPlugins(ctx); err != nil {
		return err
	}
	a.Added = a.AddedDirs()
	_, err = a.FireSession(ctx, a.Session, agent.DirectoryAdded, source, map[string]any{"directory": dir, "source": source})
	return err
}

// Directories added so far
func (a *App) AddedDirs() []string {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	return append([]string(nil), a.lib.added...)
}

// Reads the usage counts of the project
func (a *App) readUsage() map[string]int {
	usage := map[string]int{}
	dir, err := StatePath(a.Root)
	if err != nil {
		return usage
	}
	data, err := os.ReadFile(filepath.Join(dir, usageFile))
	if err != nil {
		return usage
	}
	json.Unmarshal(data, &usage)
	return usage
}

// Counts an invocation of a skill and writes the counts
func (a *App) used(name string) {
	a.lib.mu.Lock()
	a.lib.usage[name]++
	snapshot := make(map[string]int, len(a.lib.usage))
	for k, v := range a.lib.usage {
		snapshot[k] = v
	}
	a.lib.mu.Unlock()
	dir, err := StatePath(a.Root)
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(dir, 0o755) == nil {
		os.WriteFile(filepath.Join(dir, usageFile), data, 0o644)
	}
}

// Usage counts by skill name
func (a *App) skillUsage() map[string]int {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	out := make(map[string]int, len(a.lib.usage))
	for k, v := range a.lib.usage {
		out[k] = v
	}
	return out
}

// Hooks in force for a session, the settings ones, the plugin ones, the kind ones and the ones skills registered
func (a *App) hooksFor(sess *session.Session, kind Kind) agent.HookConfig {
	a.lib.mu.RLock()
	defer a.lib.mu.RUnlock()
	configs := []agent.HookConfig{a.Config.Hooks.Labeled(agent.SourceSettings, "", nil), a.lib.pluginHooks}
	if kind.Name != "" && !kind.HookConfig.Empty() {
		configs = append(configs, kind.HookConfig)
	}
	if sess != nil {
		configs = append(configs, a.lib.sessionHooks[sess.ID()]...)
	}
	return agent.MergeHooks(configs...)
}

// Starts the monitors waiting on a skill and registers its hooks for the rest of the session
func (a *App) skillInvoked(sess *session.Session, s *plugin.Skill) {
	a.startMonitors("on-skill-invoke:" + s.Short)
	a.startMonitors("on-skill-invoke:" + s.Name)
	a.registerSkillHooks(sess, s)
}

// Skill a plugin runs once after its install, false when it names none
func (a *App) OnboardingSkill(id string) (*plugin.Skill, bool) {
	lib := a.Library()
	if lib == nil {
		return nil, false
	}
	p, ok := lib.Plugin(id)
	if !ok || p.Manifest.OnboardingSkill == "" {
		return nil, false
	}
	want := filepath.Clean(filepath.Join(p.Root, p.Manifest.OnboardingSkill))
	for _, s := range p.Skills {
		if filepath.Clean(s.File) == want || filepath.Clean(s.Dir) == want {
			return s, true
		}
	}
	return nil, false
}

// Registers the hooks of a skill for the rest of the session
func (a *App) registerSkillHooks(sess *session.Session, s *plugin.Skill) {
	if sess == nil || s.Hooks.Empty() {
		return
	}
	count, chars := 0, 0
	for _, groups := range s.Hooks {
		for _, g := range groups {
			count += len(g.Hooks)
			chars += len(g.Matcher) * len(g.Hooks)
		}
	}
	if count > 1024 || chars > 16384 {
		a.send(NoticeMsg{Level: Warn, Text: "skill " + s.Name + " registers none of its hooks, it declares more than the limit allows"})
		return
	}
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	for _, have := range a.lib.sessionHooks[sess.ID()] {
		if jsonOf(have) == jsonOf(s.Hooks) {
			return
		}
	}
	a.lib.sessionHooks[sess.ID()] = append(a.lib.sessionHooks[sess.ID()], s.Hooks)
}

func jsonOf(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Marks a file or skill loaded into the session and reports whether it already was
func (a *App) markLoaded(sess *session.Session, key string) bool {
	if sess == nil {
		return false
	}
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	set := a.lib.loaded[sess.ID()]
	if set == nil {
		set = map[string]bool{}
		a.lib.loaded[sess.ID()] = set
	}
	if set[key] {
		return true
	}
	set[key] = true
	return false
}

// Forgets what a session loaded, done when it is cleared or compacted
func (a *App) forgetLoaded(sess *session.Session) {
	if sess == nil {
		return
	}
	a.lib.mu.Lock()
	delete(a.lib.loaded, sess.ID())
	delete(a.lib.content, sess.ID())
	a.lib.mu.Unlock()
}

// Records the content a skill put into the session, reporting whether the same content already sits there
func (a *App) sameContent(sess *session.Session, name, text string) bool {
	if sess == nil {
		return false
	}
	digest := plugin.Digest(text)
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	m := a.lib.content[sess.ID()]
	if m == nil {
		m = map[string]string{}
		a.lib.content[sess.ID()] = m
	}
	if m[name] == digest {
		return true
	}
	m[name] = digest
	return false
}

// Queues context for the next run of a session
func (a *App) queue(sess *session.Session, parts ...protocol.Part) {
	if sess == nil || len(parts) == 0 {
		return
	}
	a.lib.mu.Lock()
	a.lib.pending[sess.ID()] = append(a.lib.pending[sess.ID()], parts...)
	a.lib.mu.Unlock()
}

// Takes the context queued for a session
func (a *App) takeQueued(sess *session.Session) []protocol.Part {
	if sess == nil {
		return nil
	}
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	parts := a.lib.pending[sess.ID()]
	delete(a.lib.pending, sess.ID())
	return parts
}

// Hands context to a running root run of the session, or queues it for the next one
func (a *App) deliver(sess *session.Session, text string) {
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	part := protocol.Text{Text: text}
	for _, r := range a.Control.Runs() {
		if r.Running() && r.Parent == "" && r.Session == sess {
			if a.Control.Steer(r.ID, part) == nil {
				return
			}
		}
	}
	a.queue(sess, part)
}

// Output style in force, the forced one of a plugin, else the configured one
func (a *App) styleFor() (*plugin.Style, error) {
	lib := a.Library()
	if lib == nil {
		return nil, nil
	}
	name := lib.ForcedStyle
	if name == "" {
		name = a.Config.OutputStyle
	}
	if name == "" {
		return nil, nil
	}
	s, ok := lib.Style(name)
	if !ok {
		var names []string
		for _, have := range lib.Styles {
			names = append(names, have.Name)
		}
		return nil, fmt.Errorf("output style %q is not known, the styles are %s", name, strings.Join(names, ", "))
	}
	return s, nil
}

// Names of every output style known
func (a *App) StyleNames() []string {
	lib := a.Library()
	if lib == nil {
		return nil
	}
	var out []string
	for _, s := range lib.Styles {
		out = append(out, s.Name)
	}
	return out
}

// Problems the library found at the last load
func (a *App) LibraryProblems() []string {
	lib := a.Library()
	if lib == nil {
		return nil
	}
	return append([]string(nil), lib.Problems...)
}

// Error of a plugin operation the store refused
var ErrNoStore = errors.New("harness: the plugin store is not open")
