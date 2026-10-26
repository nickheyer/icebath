package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// One value at a path inside a configuration file
type Field struct {
	Path  []string
	Value any
	// Removes the value from every file holding it instead of writing one
	Unset bool
}

// Applies a setting and saves it so the next start under dir reads the same value
func (c *Config) SaveSetting(dir, key, value string) error {
	f, err := c.settingField(key, value)
	if err != nil {
		return err
	}
	return Save(dir, c.Sources, f)
}

// Writes each value into every file setting it, else the user file, and removes unset ones from every file
func Save(dir string, sources []string, fields ...Field) error {
	layers, err := configLayers(dir)
	if err != nil {
		return err
	}
	var files []string
	docs := map[string]*object{}
	for _, path := range append(layers, sources...) {
		if _, ok := docs[path]; ok {
			continue
		}
		doc, err := readObject(path)
		if err != nil {
			return err
		}
		docs[path] = doc
		files = append(files, path)
	}
	changed := map[string]bool{}
	for _, f := range fields {
		var targets []string
		for _, path := range files {
			if docs[path].holds(f.Path) {
				targets = append(targets, path)
			}
		}
		if f.Unset {
			for _, path := range targets {
				if err := docs[path].remove(f.Path); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				changed[path] = true
			}
			continue
		}
		if len(targets) == 0 {
			targets = layers[:1]
		}
		for _, path := range targets {
			if err := docs[path].place(f.Path, f.Value); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			changed[path] = true
		}
	}
	for _, path := range files {
		if !changed[path] {
			continue
		}
		if err := writeObject(path, docs[path]); err != nil {
			return err
		}
	}
	return nil
}

// JSON map that keeps its keys in file order and its values as written
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// Reads the top level keys of JSON in order, leaving every value raw
func decodeObject(data []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	o := newObject()
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// Replaces a value in place or appends it as the last key
func (o *object) set(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

// Writes the keys in their kept order
func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := encode(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Reports whether the object holds a value at the field path
func (o *object) holds(path []string) bool {
	raw, ok := o.vals[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	inner, err := decodeObject(raw)
	return err == nil && inner.holds(path[1:])
}

// Sets the value at the field path, creating the objects along it
func (o *object) place(path []string, value any) error {
	if len(path) == 1 {
		raw, err := encode(value)
		if err != nil {
			return err
		}
		o.set(path[0], raw)
		return nil
	}
	inner := newObject()
	if raw, ok := o.vals[path[0]]; ok && string(bytes.TrimSpace(raw)) != "null" {
		var err error
		if inner, err = decodeObject(raw); err != nil {
			return fmt.Errorf("%s is not an object", path[0])
		}
	}
	if err := inner.place(path[1:], value); err != nil {
		return err
	}
	raw, err := encode(inner)
	if err != nil {
		return err
	}
	o.set(path[0], raw)
	return nil
}

// Drops the value at the field path, and any object along it left empty
func (o *object) remove(path []string) error {
	raw, ok := o.vals[path[0]]
	if !ok {
		return nil
	}
	if len(path) == 1 {
		o.drop(path[0])
		return nil
	}
	inner, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("%s is not an object", path[0])
	}
	if err := inner.remove(path[1:]); err != nil {
		return err
	}
	if len(inner.keys) == 0 {
		o.drop(path[0])
		return nil
	}
	enc, err := encode(inner)
	if err != nil {
		return err
	}
	o.set(path[0], enc)
	return nil
}

// Deletes a key and its place in the order
func (o *object) drop(key string) {
	delete(o.vals, key)
	if i := slices.Index(o.keys, key); i >= 0 {
		o.keys = slices.Delete(o.keys, i, i+1)
	}
}

// Encodes a value as compact JSON with HTML characters left as they are
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Loads a configuration file in key order, empty when the file does not exist
func readObject(path string) (*object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newObject(), nil
	}
	if err != nil {
		return nil, err
	}
	o, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// Saves as indented JSON, creating the directory it lives in
func writeObject(path string, o *object) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(o); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

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
func readRules(path string) (policy.RuleSet, error) {
	if kindOf(path) == RulesFile {
		set, err := policy.LoadRulesFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return policy.RuleSet{}, nil
		}
		if err != nil {
			return policy.RuleSet{}, fmt.Errorf("%s: %w", path, err)
		}
		return set, nil
	}
	var set policy.RuleSet
	return set, readField(path, "permissions", &set)
}

func writeRules(path string, set policy.RuleSet) error {
	if kindOf(path) == RulesFile {
		return set.Save(path)
	}
	return writeField(path, "permissions", set, len(set.Allow)+len(set.Ask)+len(set.Deny) == 0)
}

// Rule lists of a set by decision
func ruleLists(s *policy.RuleSet) map[policy.Decision]*[]string {
	return map[policy.Decision]*[]string{policy.Allow: &s.Allow, policy.Ask: &s.Ask, policy.Deny: &s.Deny}
}

// Rule text in its canonical form after checking the decision and the syntax
func checkRule(d policy.Decision, rule string) (string, error) {
	if d != policy.Allow && d != policy.Ask && d != policy.Deny {
		return "", fmt.Errorf("decision must be allow, ask or deny, got %q", d)
	}
	r, err := policy.ParseRule(rule)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

// Reports whether two rule texts parse to the same rule
func sameRule(a, b string) bool {
	ra, ea := policy.ParseRule(a)
	rb, eb := policy.ParseRule(b)
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
	var set policy.RuleSet
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
func (a *App) editRules(path string, change func(*policy.RuleSet) error) error {
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
	return a.editRules(filepath.Join(dir, RulesFile), func(s *policy.RuleSet) error {
		addRule(s, d, text)
		return nil
	})
}

// Deletes a rule from the file holding it
func (a *App) RemoveRule(e RuleEntry) error {
	return a.editRules(e.Path, func(s *policy.RuleSet) error { return dropRule(s, e) })
}

// Replaces a rule in the file holding it
func (a *App) ReplaceRule(old RuleEntry, d policy.Decision, rule string) error {
	text, err := checkRule(d, rule)
	if err != nil {
		return err
	}
	return a.editRules(old.Path, func(s *policy.RuleSet) error {
		if err := dropRule(s, old); err != nil {
			return err
		}
		addRule(s, d, text)
		return nil
	})
}

func addRule(s *policy.RuleSet, d policy.Decision, text string) {
	list := ruleLists(s)[d]
	if !slices.ContainsFunc(*list, func(r string) bool { return sameRule(r, text) }) {
		*list = append(*list, text)
	}
}

func dropRule(s *policy.RuleSet, e RuleEntry) error {
	list := ruleLists(s)[e.Decision]
	if !slices.ContainsFunc(*list, func(r string) bool { return sameRule(r, e.Rule) }) {
		return fmt.Errorf("%s no longer holds %s %s", e.Path, e.Decision, e.Rule)
	}
	*list = slices.DeleteFunc(*list, func(r string) bool { return sameRule(r, e.Rule) })
	return nil
}

// One hook command with the event and matcher it runs for and the file holding it
type HookEntry struct {
	Event   agent.HookEvent
	Matcher string
	Command string
	Timeout int
	Path    string
}

// Hooks a file holds, none when it does not exist
func readHooks(path string) (agent.HookConfig, error) {
	if kindOf(path) == HooksFile {
		cfg, err := agent.LoadHooksFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return cfg, nil
	}
	var cfg agent.HookConfig
	if err := readField(path, "hooks", &cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// Writes hooks into a file, dropping groups and events left empty
func writeHooks(path string, cfg agent.HookConfig) error {
	for ev, groups := range cfg {
		groups = slices.DeleteFunc(groups, func(g agent.HookGroup) bool { return len(g.Hooks) == 0 })
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
	if !slices.Contains(agent.HookEvents, e.Event) {
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
		for _, ev := range agent.HookEvents {
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
	merged := agent.HookConfig{}
	for _, path := range paths {
		cfg, err := readHooks(path)
		if err != nil {
			return err
		}
		for _, ev := range agent.HookEvents {
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
func (a *App) editHooks(path string, change func(agent.HookConfig) error) error {
	cfg, err := readHooks(path)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = agent.HookConfig{}
	}
	if err := change(cfg); err != nil {
		return err
	}
	if err := writeHooks(path, cfg); err != nil {
		return err
	}
	return a.ReloadHooks()
}

func addHook(cfg agent.HookConfig, e HookEntry) {
	cmd := agent.HookCommand{Command: e.Command, Timeout: e.Timeout}
	for i, g := range cfg[e.Event] {
		if g.Matcher == e.Matcher {
			cfg[e.Event][i].Hooks = append(g.Hooks, cmd)
			return
		}
	}
	cfg[e.Event] = append(cfg[e.Event], agent.HookGroup{Matcher: e.Matcher, Hooks: []agent.HookCommand{cmd}})
}

func dropHook(cfg agent.HookConfig, e HookEntry) error {
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
	return a.editHooks(filepath.Join(dir, HooksFile), func(cfg agent.HookConfig) error {
		addHook(cfg, e)
		return nil
	})
}

// Deletes a hook from the file holding it
func (a *App) RemoveHook(e HookEntry) error {
	return a.editHooks(e.Path, func(cfg agent.HookConfig) error { return dropHook(cfg, e) })
}

// Replaces a hook in the file holding it
func (a *App) ReplaceHook(old, e HookEntry) error {
	if err := checkHook(e); err != nil {
		return err
	}
	return a.editHooks(old.Path, func(cfg agent.HookConfig) error {
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

// One profile a profile file holds, with its template applied and as written
type Instance struct {
	Profile profiles.Profile
	Written profiles.Profile
	Path    string
}

// Profile files in read order, the configured lists and then the user file
func profileFiles(cfg Config) ([]string, error) {
	user, err := profiles.UserPath()
	if err != nil {
		return nil, err
	}
	return append(append([]string(nil), cfg.ProfileFiles...), user), nil
}

// Profiles of every profile file in order, a later namesake replacing in place, with the last default named
func loadInstances(cfg Config) ([]Instance, string, error) {
	files, err := profileFiles(cfg)
	if err != nil {
		return nil, "", err
	}
	var out []Instance
	var def string
	for i, path := range files {
		doc, err := profiles.ReadFile(path)
		if i == len(files)-1 && errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if doc.Default != "" {
			def = doc.Default
		}
		for _, w := range doc.Profiles {
			p, err := w.Expand()
			if err != nil {
				return nil, "", err
			}
			in := Instance{Profile: p, Written: w, Path: path}
			if j := indexOf(out, p.Name); j >= 0 {
				out[j] = in
				continue
			}
			out = append(out, in)
		}
	}
	return out, def, nil
}

func indexOf(list []Instance, name string) int {
	for i, in := range list {
		if in.Profile.Name == name {
			return i
		}
	}
	return -1
}

// Profile by name, a written one before the built in one it may shadow
func (a *App) FindProfile(name string) (profiles.Profile, bool) {
	if i := indexOf(a.Profiles, name); i >= 0 {
		return a.Profiles[i].Profile, true
	}
	return profiles.Lookup(name)
}

// Written profiles in file order
func (a *App) Instances() []Instance { return append([]Instance(nil), a.Profiles...) }

// Reports whether a written profile carries the name
func (a *App) Shadowed(name string) bool { return indexOf(a.Profiles, name) >= 0 }

// Reads every profile file again
func (a *App) ReloadProfiles() error {
	list, _, err := loadInstances(a.Config)
	if err != nil {
		return err
	}
	a.Profiles = list
	return nil
}

// File holding the profile, the user file for a new one
func (a *App) profilePath(name string) (string, error) {
	if i := indexOf(a.Profiles, name); i >= 0 {
		return a.Profiles[i].Path, nil
	}
	return profiles.UserPath()
}

// Reads a profile file, empty when it does not exist yet
func readDocument(path string) (profiles.Document, error) {
	doc, err := profiles.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return profiles.Document{}, nil
	}
	return doc, err
}

// Applies a change to the file holding the profile, writes it and reads every file again
func (a *App) editProfiles(name string, change func(*profiles.Document) error) error {
	path, err := a.profilePath(name)
	if err != nil {
		return err
	}
	doc, err := readDocument(path)
	if err != nil {
		return err
	}
	if err := change(&doc); err != nil {
		return err
	}
	if err := profiles.WriteFile(path, doc); err != nil {
		return err
	}
	return a.ReloadProfiles()
}

// Writes a profile over the one named old, or adds it to the user file when old is empty
func (a *App) SaveProfile(old string, p profiles.Profile) error {
	if p.Name != old {
		if a.Shadowed(p.Name) {
			return fmt.Errorf("a profile named %s already exists", p.Name)
		}
		if _, builtin := profiles.Lookup(p.Name); builtin {
			return fmt.Errorf("%s names a built in provider, pick another name", p.Name)
		}
	}
	if old != "" && p.Disabled && a.ProfileInUse(old) {
		return fmt.Errorf("switch away from %s before turning it off", old)
	}
	e, err := p.Expand()
	if err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	return a.editProfiles(old, func(d *profiles.Document) error { return d.Put(old, p) })
}

// Deletes a written profile, refusing one in use
func (a *App) RemoveProfile(name string) error {
	if a.ProfileInUse(name) {
		return fmt.Errorf("switch away from %s before deleting it", name)
	}
	if !a.Shadowed(name) {
		return fmt.Errorf("no written profile %s", name)
	}
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Remove(name) })
}

// Moves a written profile delta places through its file
func (a *App) MoveProfile(name string, delta int) error {
	if !a.Shadowed(name) {
		return fmt.Errorf("no written profile %s", name)
	}
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Move(name, delta) })
}

// Turns a written profile on or off, refusing to turn off one in use
func (a *App) SetProfileEnabled(name string, on bool) error {
	i := indexOf(a.Profiles, name)
	if i < 0 {
		return fmt.Errorf("no written profile %s", name)
	}
	if !on && a.ProfileInUse(name) {
		return fmt.Errorf("switch away from %s before turning it off", name)
	}
	w := a.Profiles[i].Written
	w.Disabled = !on
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Put(name, w) })
}
