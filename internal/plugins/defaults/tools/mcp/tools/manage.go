package tools

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
)

// Takes every write a person asks for of the servers and of the values they read, so the page, icebath mcp and the settings service end in
// the same functions and none keeps what another would refuse
func guard(cfg *config.Config, m *Manager) {
	cfg.Guard(mcpconfig.Servers, config.Guard{Check: m.checkWrite, Write: m.write})
	cfg.Guard(mcpconfig.Env, config.Guard{Check: m.checkValue, Write: m.writeValue})
}

// Name of the server and the path of a field inside its settings of a key under the servers or the values, both empty for the whole table.
// The key comes as the settings keep it: the prefix spelled as declared, the name as written
func placed(prefix, key string) (name, path string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(key, prefix), ".")
	name, path, _ = strings.Cut(rest, ".")
	return name, path
}

// Settings with the value put under the dotted path inside them, nil taking it out
func put(settings map[string]any, path string, value any) map[string]any {
	parts := strings.Split(path, ".")
	node := settings
	for _, part := range parts[:len(parts)-1] {
		child, ok := node[part].(map[string]any)
		if !ok {
			if value == nil {
				return settings
			}
			child = map[string]any{}
			node[part] = child
		}
		node = child
	}
	if value == nil {
		delete(node, parts[len(parts)-1])
	} else {
		node[parts[len(parts)-1]] = value
	}
	return settings
}

// Settings of a server once the write of a field is made, from those it starts from
func (m *Manager) merged(cfg *config.Config, name, path string, value any) (map[string]any, error) {
	entry := m.base(cfg, name)
	if entry == nil {
		return nil, fmt.Errorf("No MCP server is called “%s”.", name)
	}
	return put(entry, path, value), nil
}

// Why a write of a key under the servers would be refused, nil when it would be kept: the name and the shape of every server it saves
func (m *Manager) checkWrite(cfg *config.Config, key string, value any) error {
	name, path := placed(mcpconfig.Servers, key)
	switch {
	case name == "":
		table, _ := value.(map[string]any)
		for each, settings := range table {
			entry, _ := settings.(map[string]any)
			if issues := m.Check(each, normal(entry)); len(issues) > 0 {
				return errors.New(strings.Join(issues, " "))
			}
		}
		return nil
	case path == "" && value == nil:
		return nil
	case path == "":
		entry, _ := value.(map[string]any)
		if issues := m.Check(name, normal(entry)); len(issues) > 0 {
			return errors.New(strings.Join(issues, " "))
		}
		return nil
	}
	entry, err := m.merged(cfg, name, path, value)
	if err != nil {
		return err
	}
	if issues := m.Check(name, normal(entry)); len(issues) > 0 {
		return errors.New(strings.Join(issues, " "))
	}
	return nil
}

// Makes a write of a key under the servers through the functions of what it changes: a server saved whole or one field of it, removed, or
// every server at once
func (m *Manager) write(cfg *config.Config, key string, value any) (bool, error) {
	name, path := placed(mcpconfig.Servers, key)
	switch {
	case name == "":
		if err := m.checkWrite(cfg, key, value); err != nil {
			return true, err
		}
		next, _ := value.(map[string]any)
		for _, existing := range m.StoredNames(cfg) {
			if _, kept := next[existing]; !kept {
				if _, err := m.Remove(cfg, existing); err != nil {
					return true, err
				}
			}
		}
		for _, each := range slices.Sorted(maps.Keys(next)) {
			entry, _ := next[each].(map[string]any)
			if err := m.saved(cfg, each, entry); err != nil {
				return true, err
			}
		}
		return true, nil
	case path == "" && value == nil:
		_, err := m.Remove(cfg, name)
		return true, err
	case path == "":
		entry, _ := value.(map[string]any)
		return true, m.saved(cfg, name, entry)
	}
	entry, err := m.merged(cfg, name, path, value)
	if err != nil {
		return true, err
	}
	return true, m.saved(cfg, name, entry)
}

// Saves a server, its issues as the error that refused it
func (m *Manager) saved(cfg *config.Config, name string, settings map[string]any) error {
	issues, err := m.Save(cfg, name, settings)
	if err == nil && len(issues) > 0 {
		err = errors.New(strings.Join(issues, " "))
	}
	return err
}

// Why a write of a key under the values would be refused: a name a value can't have
func (m *Manager) checkValue(_ *config.Config, key string, value any) error {
	name, _ := placed(mcpconfig.Env, key)
	if name != "" {
		return checkName(name)
	}
	table, _ := value.(map[string]any)
	for each := range table {
		if err := checkName(each); err != nil {
			return err
		}
	}
	return nil
}

func checkName(name string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("“%s” cannot name a value. Use letters, digits and underscores, and do not start with a digit.", name)
	}
	return nil
}

// Makes a write of a key under the values through the one function that sets and removes them
func (m *Manager) writeValue(cfg *config.Config, key string, value any) (bool, error) {
	name, _ := placed(mcpconfig.Env, key)
	if name != "" {
		text := ""
		if value != nil {
			text = fmt.Sprint(value)
		}
		return true, m.SetValue(cfg, name, text)
	}
	if err := m.checkValue(cfg, key, value); err != nil {
		return true, err
	}
	kept := table(cfg.Get(mcpconfig.Env))
	next, _ := value.(map[string]any)
	for each := range kept {
		if _, stays := next[each]; !stays {
			if err := m.SetValue(cfg, each, ""); err != nil {
				return true, err
			}
		}
	}
	for each, v := range next {
		if err := m.SetValue(cfg, each, fmt.Sprint(v)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// One settings section for each server of the profile, holding the fields of its settings by the way it connects, and one for each group
// of its fields with a page of its own. Made from what the file keeps and what the plugins shipped when the servers were last read, since
// reading any setting looks through these sections
type sections struct{ m *Manager }

func (s sections) Sections(values *config.Config) []config.Section {
	stored, _ := values.Stored(mcpconfig.Servers).(map[string]any)
	servers := map[string]map[string]any{}
	for name, value := range stored {
		if entry, ok := value.(map[string]any); ok && config.Named(name) {
			servers[name] = entry
		}
	}
	plugins := map[string]string{}
	if p := s.m.existing(values.Profile()); p != nil {
		for _, entry := range p.shipped() {
			if _, kept := servers[entry.Name]; !kept {
				servers[entry.Name], plugins[entry.Name] = entryConfig(entry, ""), entry.Plugin
			}
		}
	}
	var out []config.Section
	for place, name := range slices.Sorted(maps.Keys(servers)) {
		raw := servers[name]
		key := mcpconfig.Servers + "." + name
		// The address of a server the file keeps read by its key, which answers to it in any case while these sections are made
		remote := raw[mcpconfig.URL] != nil || plugins[name] == "" && values.Stored(key+"."+mcpconfig.URL) != nil
		about := "An MCP server you added"
		if plugin := plugins[name]; plugin != "" {
			about = "An MCP server the plugin " + plugin + " ships"
		}
		fields := mcpconfig.Fields(key, remote)
		if plugins[name] != "" {
			fields = shipped(fields, key, raw)
		}
		out = append(out, config.Section{Key: key, Parent: mcpconfig.Key, Title: name, About: about, Order: (place + 1) * 10, Settings: fields})
		for order, part := range mcpconfig.Parts(remote) {
			out = append(out, config.Section{Key: key + "." + part.Key, Parent: key, Title: part.Title, About: part.About, Order: (order + 1) * 10,
				Settings: part.Fields(key + "." + part.Key)})
		}
	}
	return out
}

// Fields of a server a plugin ships, each defaulting to what the plugin gives it, so what is shown is what runs until it is changed
func shipped(fields []config.Setting, key string, raw map[string]any) []config.Setting {
	out := slices.Clone(fields)
	for i, field := range out {
		value, ok := raw[strings.TrimPrefix(field.Key, key+".")]
		if !ok {
			continue
		}
		switch field.Default.(type) {
		case string:
			if text, ok := value.(string); ok {
				out[i].Default = text
			}
		case []string:
			out[i].Default = stringList(value)
		case map[string]any:
			if group, ok := value.(map[string]any); ok {
				out[i].Default = group
			}
		}
	}
	return out
}

// Entries of the catalog plugins ship that the profile's servers were last read with
func (p *pool) shipped() []mcp.Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bundled)
}

// How a server stands, in the words every page, command and message says it
func (s Status) Words() string {
	switch {
	case s.State == "disabled":
		return "Off"
	case s.State == "connected":
		return "Connected"
	case s.State == "failed":
		return "Failed"
	case len(s.Missing) > 0:
		return "Needs values"
	case s.OAuth && !s.SignedIn:
		return "Not signed in"
	}
	return map[string]string{"configured": "Not connected", "connecting": "Connecting", "lazy": "Starts on first use",
		"recycled": "Resting until used"}[s.State]
}

// Names of the values a server's settings read as ${NAME}, the names of the context and of its folders left out, each once
func reads(raw any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, m := range reference.FindAllStringSubmatch(x, -1) {
				name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[1]), "env:"))
				if !slices.Contains(given, name) && !slices.Contains(out, name) && envName.MatchString(name) {
					out = append(out, name)
				}
			}
		case map[string]any:
			for _, item := range x {
				walk(item)
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(raw)
	slices.Sort(out)
	return out
}

// Names a server's settings read that the context gives, never a value of yours
var given = []string{"userHome", "workspaceFolder", "workspaceFolderBasename", "pathSeparator", "INSTALL_DIR"}
