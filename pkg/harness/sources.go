package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Where a new entry is written, the project directory or the user configuration directory
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// Scopes a new entry may be written to, the project first
var Scopes = []Scope{ScopeProject, ScopeUser}

// Directory a scope keeps its files in
func (a *App) scopeDir(s Scope) (string, error) {
	switch s {
	case ScopeUser:
		return profiles.ConfigDir()
	case ScopeProject:
		return filepath.Join(a.Dir, ProjectDir), nil
	}
	return "", fmt.Errorf("unknown scope %q", s)
}

// Scope a file belongs to, its path when it is neither scope directory
func (a *App) ScopeOf(path string) string {
	for _, s := range Scopes {
		if dir, err := a.scopeDir(s); err == nil && filepath.Dir(path) == dir {
			return string(s)
		}
	}
	return path
}

// Files an editor of the kind reads, the config layers, that kind in both scopes and any other source
func (a *App) files(kind string) ([]string, error) {
	var out []string
	add := func(p string) {
		if k := kindOf(p); (k == kind || k == ConfigFile) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	layers, err := configLayers(a.Dir)
	if err != nil {
		return nil, err
	}
	for _, p := range layers {
		add(p)
	}
	for _, s := range []Scope{ScopeUser, ScopeProject} {
		dir, err := a.scopeDir(s)
		if err != nil {
			return nil, err
		}
		add(filepath.Join(dir, kind))
	}
	for _, p := range a.Config.Sources {
		add(p)
	}
	return out, nil
}

// Kind of configuration a file holds, rules, hooks or a whole configuration
func kindOf(path string) string {
	switch filepath.Base(path) {
	case RulesFile, HooksFile:
		return filepath.Base(path)
	}
	return ConfigFile
}

// Decodes one top level field of a JSON file, leaving v alone when the file or field is missing
func readField(path, key string, v any) error {
	o, err := readObject(path)
	if err != nil {
		return err
	}
	raw, ok := o.vals[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %s: %w", path, key, err)
	}
	return nil
}

// Writes one top level field of a JSON file, dropping it when gone is true
func writeField(path, key string, v any, gone bool) error {
	o, err := readObject(path)
	if err != nil {
		return err
	}
	if gone {
		o.drop(key)
	} else if err := o.place([]string{key}, v); err != nil {
		return err
	}
	return writeObject(path, o)
}

// One permission rule with its decision and the file holding it
type RuleEntry struct {
	Decision policy.Decision
	Rule     string
	Path     string
}

// Rules a file holds, none when it does not exist
func readRules(path string) (rules.Set, error) {
	if kindOf(path) == RulesFile {
		set, err := rules.LoadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return rules.Set{}, nil
		}
		if err != nil {
			return rules.Set{}, fmt.Errorf("%s: %w", path, err)
		}
		return set, nil
	}
	var set rules.Set
	return set, readField(path, "permissions", &set)
}

func writeRules(path string, set rules.Set) error {
	if kindOf(path) == RulesFile {
		return set.Save(path)
	}
	return writeField(path, "permissions", set, len(set.Allow)+len(set.Ask)+len(set.Deny) == 0)
}

// Rule lists of a set by decision
func ruleLists(s *rules.Set) map[policy.Decision]*[]string {
	return map[policy.Decision]*[]string{policy.Allow: &s.Allow, policy.Ask: &s.Ask, policy.Deny: &s.Deny}
}

// Rule text in its canonical form after checking the decision and the syntax
func checkRule(d policy.Decision, rule string) (string, error) {
	if d != policy.Allow && d != policy.Ask && d != policy.Deny {
		return "", fmt.Errorf("decision must be allow, ask or deny, got %q", d)
	}
	r, err := rules.Parse(rule)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

// Reports whether two rule texts parse to the same rule
func sameRule(a, b string) bool {
	ra, ea := rules.Parse(a)
	rb, eb := rules.Parse(b)
	return ea == nil && eb == nil && ra == rb
}

// Every rule the configuration files hold, in read order
func (a *App) RuleEntries() ([]RuleEntry, error) {
	paths, err := a.files(RulesFile)
	if err != nil {
		return nil, err
	}
	var out []RuleEntry
	for _, path := range paths {
		set, err := readRules(path)
		if err != nil {
			return nil, err
		}
		for _, d := range []policy.Decision{policy.Deny, policy.Ask, policy.Allow} {
			for _, r := range *ruleLists(&set)[d] {
				out = append(out, RuleEntry{Decision: d, Rule: r, Path: path})
			}
		}
	}
	return out, nil
}

// Reads every rule file again and puts its rules in force
func (a *App) ReloadRules() error {
	entries, err := a.RuleEntries()
	if err != nil {
		return err
	}
	var set rules.Set
	for _, e := range entries {
		list := ruleLists(&set)[e.Decision]
		*list = append(*list, e.Rule)
	}
	if err := a.Rules.Reset(set); err != nil {
		return err
	}
	a.Config.Permissions = set
	return nil
}

// Applies a change to the rules of one file, writes it and puts every rule in force again
func (a *App) editRules(path string, change func(*rules.Set) error) error {
	set, err := readRules(path)
	if err != nil {
		return err
	}
	if err := change(&set); err != nil {
		return err
	}
	if err := writeRules(path, set); err != nil {
		return err
	}
	return a.ReloadRules()
}

// Adds a rule to the rules file of the scope
func (a *App) AddRule(scope Scope, d policy.Decision, rule string) error {
	text, err := checkRule(d, rule)
	if err != nil {
		return err
	}
	dir, err := a.scopeDir(scope)
	if err != nil {
		return err
	}
	return a.editRules(filepath.Join(dir, RulesFile), func(s *rules.Set) error {
		addRule(s, d, text)
		return nil
	})
}

// Deletes a rule from the file holding it
func (a *App) RemoveRule(e RuleEntry) error {
	return a.editRules(e.Path, func(s *rules.Set) error { return dropRule(s, e) })
}

// Replaces a rule in the file holding it
func (a *App) ReplaceRule(old RuleEntry, d policy.Decision, rule string) error {
	text, err := checkRule(d, rule)
	if err != nil {
		return err
	}
	return a.editRules(old.Path, func(s *rules.Set) error {
		if err := dropRule(s, old); err != nil {
			return err
		}
		addRule(s, d, text)
		return nil
	})
}

func addRule(s *rules.Set, d policy.Decision, text string) {
	list := ruleLists(s)[d]
	if !slices.ContainsFunc(*list, func(r string) bool { return sameRule(r, text) }) {
		*list = append(*list, text)
	}
}

func dropRule(s *rules.Set, e RuleEntry) error {
	list := ruleLists(s)[e.Decision]
	if !slices.ContainsFunc(*list, func(r string) bool { return sameRule(r, e.Rule) }) {
		return fmt.Errorf("%s no longer holds %s %s", e.Path, e.Decision, e.Rule)
	}
	*list = slices.DeleteFunc(*list, func(r string) bool { return sameRule(r, e.Rule) })
	return nil
}

// One hook command with the event and matcher it runs for and the file holding it
type HookEntry struct {
	Event   hooks.Event
	Matcher string
	Command string
	Timeout int
	Path    string
}

// Hooks a file holds, none when it does not exist
func readHooks(path string) (hooks.Config, error) {
	if kindOf(path) == HooksFile {
		cfg, err := hooks.LoadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return cfg, nil
	}
	var cfg hooks.Config
	if err := readField(path, "hooks", &cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// Writes hooks into a file, dropping groups and events left empty
func writeHooks(path string, cfg hooks.Config) error {
	for ev, groups := range cfg {
		groups = slices.DeleteFunc(groups, func(g hooks.Group) bool { return len(g.Hooks) == 0 })
		if len(groups) == 0 {
			delete(cfg, ev)
			continue
		}
		cfg[ev] = groups
	}
	if kindOf(path) == HooksFile {
		o := newObject()
		if err := o.place([]string{"hooks"}, cfg); err != nil {
			return err
		}
		return writeObject(path, o)
	}
	return writeField(path, "hooks", cfg, len(cfg) == 0)
}

// Checks that a matcher compiles as a regular expression
func CheckMatcher(m string) error {
	if _, err := regexp.Compile(m); err != nil {
		return fmt.Errorf("matcher %q does not compile, %w", m, err)
	}
	return nil
}

// Checks the event, the matcher pattern, the command and the timeout of a hook
func checkHook(e HookEntry) error {
	if !slices.Contains(hooks.Events, e.Event) {
		return fmt.Errorf("unknown hook event %q", e.Event)
	}
	if strings.TrimSpace(e.Command) == "" {
		return errors.New("a hook needs a command")
	}
	if err := CheckMatcher(e.Matcher); err != nil {
		return err
	}
	if e.Timeout < 0 {
		return errors.New("timeout must not be negative")
	}
	return nil
}

// Every hook the configuration files hold, in read order and event order
func (a *App) HookEntries() ([]HookEntry, error) {
	paths, err := a.files(HooksFile)
	if err != nil {
		return nil, err
	}
	var out []HookEntry
	for _, path := range paths {
		cfg, err := readHooks(path)
		if err != nil {
			return nil, err
		}
		for _, ev := range hooks.Events {
			for _, g := range cfg[ev] {
				for _, h := range g.Hooks {
					out = append(out, HookEntry{Event: ev, Matcher: g.Matcher, Command: h.Command, Timeout: h.Timeout, Path: path})
				}
			}
		}
	}
	return out, nil
}

// Reads every hook file again and puts its hooks in force for later runs
func (a *App) ReloadHooks() error {
	paths, err := a.files(HooksFile)
	if err != nil {
		return err
	}
	merged := hooks.Config{}
	for _, path := range paths {
		cfg, err := readHooks(path)
		if err != nil {
			return err
		}
		for _, ev := range hooks.Events {
			merged[ev] = append(merged[ev], cfg[ev]...)
		}
	}
	for ev, groups := range merged {
		if len(groups) == 0 {
			delete(merged, ev)
		}
	}
	if err := merged.Validate(); err != nil {
		return err
	}
	a.Config.Hooks = merged
	return nil
}

// Applies a change to the hooks of one file, writes it and puts every hook in force again
func (a *App) editHooks(path string, change func(hooks.Config) error) error {
	cfg, err := readHooks(path)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = hooks.Config{}
	}
	if err := change(cfg); err != nil {
		return err
	}
	if err := writeHooks(path, cfg); err != nil {
		return err
	}
	return a.ReloadHooks()
}

func addHook(cfg hooks.Config, e HookEntry) {
	cmd := hooks.Command{Command: e.Command, Timeout: e.Timeout}
	for i, g := range cfg[e.Event] {
		if g.Matcher == e.Matcher {
			cfg[e.Event][i].Hooks = append(g.Hooks, cmd)
			return
		}
	}
	cfg[e.Event] = append(cfg[e.Event], hooks.Group{Matcher: e.Matcher, Hooks: []hooks.Command{cmd}})
}

func dropHook(cfg hooks.Config, e HookEntry) error {
	for i, g := range cfg[e.Event] {
		if g.Matcher != e.Matcher {
			continue
		}
		for j, h := range g.Hooks {
			if h.Command == e.Command && h.Timeout == e.Timeout {
				cfg[e.Event][i].Hooks = slices.Delete(g.Hooks, j, j+1)
				return nil
			}
		}
	}
	return fmt.Errorf("%s no longer holds that %s hook", e.Path, e.Event)
}

// Adds a hook to the hooks file of the scope
func (a *App) AddHook(scope Scope, e HookEntry) error {
	if err := checkHook(e); err != nil {
		return err
	}
	dir, err := a.scopeDir(scope)
	if err != nil {
		return err
	}
	return a.editHooks(filepath.Join(dir, HooksFile), func(cfg hooks.Config) error {
		addHook(cfg, e)
		return nil
	})
}

// Deletes a hook from the file holding it
func (a *App) RemoveHook(e HookEntry) error {
	return a.editHooks(e.Path, func(cfg hooks.Config) error { return dropHook(cfg, e) })
}

// Replaces a hook in the file holding it
func (a *App) ReplaceHook(old, e HookEntry) error {
	if err := checkHook(e); err != nil {
		return err
	}
	return a.editHooks(old.Path, func(cfg hooks.Config) error {
		if err := dropHook(cfg, old); err != nil {
			return err
		}
		addHook(cfg, e)
		return nil
	})
}

// One MCP server with the file configuring it
type ServerEntry struct {
	Config ServerConfig
	Path   string
}

func readServers(path string) ([]ServerConfig, error) {
	var list []ServerConfig
	return list, readField(path, "mcp", &list)
}

func writeServers(path string, list []ServerConfig) error {
	return writeField(path, "mcp", list, len(list) == 0)
}

// Servers the configuration files hold, a later file replacing an earlier namesake in place
func (a *App) ServerEntries() ([]ServerEntry, error) {
	paths, err := a.files(ConfigFile)
	if err != nil {
		return nil, err
	}
	var out []ServerEntry
	for _, path := range paths {
		list, err := readServers(path)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			e := ServerEntry{Config: s, Path: path}
			if i := slices.IndexFunc(out, func(o ServerEntry) bool { return o.Config.Name == s.Name }); i >= 0 {
				out[i] = e
				continue
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// Entry of the server with the name, false when no file configures it
func (a *App) serverEntry(name string) (ServerEntry, bool, error) {
	entries, err := a.ServerEntries()
	if err != nil {
		return ServerEntry{}, false, err
	}
	for _, e := range entries {
		if e.Config.Name == name {
			return e, true, nil
		}
	}
	return ServerEntry{}, false, nil
}

// Applies a change to the servers of one file and writes it
func editServers(path string, change func([]ServerConfig) ([]ServerConfig, error)) error {
	list, err := readServers(path)
	if err != nil {
		return err
	}
	if list, err = change(list); err != nil {
		return err
	}
	return writeServers(path, list)
}

// Closes the server named old and opens it under cfg unless turned off, blocking while the old one exits
func (a *App) RestartServer(ctx context.Context, old string, cfg ServerConfig) error {
	if err := a.CloseServer(old); err != nil {
		return err
	}
	return a.MCP.Add(ctx, cfg)
}

// Closes and forgets the server when it is open, waiting for it to exit
func (a *App) CloseServer(name string) error {
	if name == "" || !slices.ContainsFunc(a.MCP.Status(), func(s ServerStatus) bool { return s.Name == name }) {
		return nil
	}
	return a.MCP.Remove(name)
}

// Writes a server over old in its file or into the config file of the scope, without restarting it
func (a *App) SaveServer(scope Scope, old string, cfg ServerConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("an mcp server needs a name")
	}
	if _, err := cfg.Dialer(); err != nil {
		return err
	}
	if cfg.Name != old {
		_, taken, err := a.serverEntry(cfg.Name)
		if err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("an mcp server named %s already exists", cfg.Name)
		}
	}
	path := ""
	if old != "" {
		e, ok, err := a.serverEntry(old)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no mcp server %s", old)
		}
		path = e.Path
	} else {
		dir, err := a.scopeDir(scope)
		if err != nil {
			return err
		}
		path = filepath.Join(dir, ConfigFile)
	}
	err := editServers(path, func(list []ServerConfig) ([]ServerConfig, error) {
		if i := slices.IndexFunc(list, func(s ServerConfig) bool { return s.Name == old }); old != "" && i >= 0 {
			list[i] = cfg
			return list, nil
		}
		return append(list, cfg), nil
	})
	if err != nil {
		return err
	}
	return a.syncServers()
}

// Deletes a server from every file configuring it, leaving the close to CloseServer
func (a *App) RemoveServer(name string) error {
	paths, err := a.files(ConfigFile)
	if err != nil {
		return err
	}
	for _, path := range paths {
		list, err := readServers(path)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(list, func(s ServerConfig) bool { return s.Name == name }) {
			continue
		}
		if err := writeServers(path, slices.DeleteFunc(list, func(s ServerConfig) bool { return s.Name == name })); err != nil {
			return err
		}
	}
	return a.syncServers()
}

// Turns a server on or off in the file configuring it, returning the settings RestartServer opens it under
func (a *App) SetServerEnabled(name string, on bool) (ServerConfig, error) {
	e, ok, err := a.serverEntry(name)
	if err != nil {
		return ServerConfig{}, err
	}
	if !ok {
		return ServerConfig{}, fmt.Errorf("no mcp server %s", name)
	}
	cfg := e.Config
	cfg.Disabled = !on
	err = editServers(e.Path, func(list []ServerConfig) ([]ServerConfig, error) {
		for i := range list {
			if list[i].Name == name {
				list[i] = cfg
			}
		}
		return list, nil
	})
	if err != nil {
		return ServerConfig{}, err
	}
	return cfg, a.syncServers()
}

// Keeps the configuration's server list in step with the files
func (a *App) syncServers() error {
	entries, err := a.ServerEntries()
	if err != nil {
		return err
	}
	a.Config.MCP = a.Config.MCP[:0]
	for _, e := range entries {
		a.Config.MCP = append(a.Config.MCP, e.Config)
	}
	return nil
}

// Turns a tool on or off and saves the names turned off
func (a *App) SetToolEnabled(name string, on bool) error {
	a.Tools.SetEnabled(name, on)
	off := a.Tools.Disabled()
	a.Config.Tools.Disabled = off
	f := Field{Path: []string{"tools", "disabled"}, Value: off}
	if len(off) == 0 {
		f = Field{Path: f.Path, Unset: true}
	}
	return Save(a.Dir, a.Config.Sources, f)
}
