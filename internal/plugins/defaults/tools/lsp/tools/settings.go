package tools

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

// Shortest idle time a server is stopped after, in seconds
const minIdle = 30.0

// One entry of the servers setting, by id
type serverEntry struct {
	id  string
	raw map[string]any
}

// The language server settings of a profile as a service runs by them
type settings struct {
	enabled  bool
	waitMode string
	// Seconds
	wait, warmup, retry, idle float64
	strategy                  string
	// Exclude globs, and whether the setting is no list, which keeps every workspace out
	excludes       []string
	excludeInvalid bool
	bundle         string
	entries        []serverEntry
	commands       map[string][]string
	envs           map[string]map[string]string
	inits          map[string]map[string]any
	disabled       map[string]bool
}

// Seconds of a number setting, none below zero
func seconds(cfg *config.Config, key string) float64 {
	return max(cast.ToFloat64(cfg.Get(key)), 0)
}

func duration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Settings of the profile, the servers table read as its file writes it
func (m *Manager) read(cfg *config.Config) settings {
	s := settings{enabled: cfg.GetBool(lspconfig.Enabled), waitMode: cfg.GetString(lspconfig.WaitMode), wait: seconds(cfg, lspconfig.WaitTimeout),
		warmup: seconds(cfg, lspconfig.WarmupTimeout), retry: seconds(cfg, lspconfig.BrokenRetry), idle: seconds(cfg, lspconfig.IdleTimeout),
		strategy: cfg.GetString(lspconfig.InstallStrategy), bundle: cfg.GetString(lspconfig.PowerShellBundle), commands: map[string][]string{},
		envs: map[string]map[string]string{}, inits: map[string]map[string]any{}, disabled: map[string]bool{}}
	if s.waitMode != lspconfig.WaitDocument && s.waitMode != lspconfig.WaitFull {
		s.waitMode = lspconfig.WaitDocument
	}
	if s.idle > 0 && s.idle < minIdle {
		s.idle = minIdle
	}
	excludes, ok := tools.Folders(cfg, lspconfig.ExcludeRoots)
	for i, pattern := range excludes {
		excludes[i] = terminal.ExpandUser(pattern)
	}
	s.excludes, s.excludeInvalid = excludes, !ok
	s.entries = m.servers.read(cfg)
	for _, e := range s.entries {
		if command, ok := e.raw["command"].([]any); ok && len(command) > 0 {
			parts := make([]string, len(command))
			for i, part := range command {
				parts[i] = fmt.Sprint(part)
			}
			s.commands[e.id] = parts
		}
		if env, ok := e.raw["env"].(map[string]any); ok {
			vars := map[string]string{}
			for k, v := range env {
				vars[k] = fmt.Sprint(v)
			}
			s.envs[e.id] = vars
		}
		if init, ok := e.raw["initialization_options"].(map[string]any); ok {
			s.inits[e.id] = init
		}
		if truthy(e.raw["disabled"]) {
			s.disabled[e.id] = true
		}
	}
	return s
}

// Whether a value written as on, yes, true or a number other than zero says so
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return config.Yes(t)
	case int, int64, float64:
		return cast.ToFloat64(t) != 0
	}
	return false
}

// Whether two readings of the settings start the same service
func (s settings) same(other settings) bool { return reflect.DeepEqual(s, other) }

// Servers table of each profile file as last read, read again once the file changed
type serversFile struct {
	mu    sync.Mutex
	cache map[string]serversRead
}

// The table as a reading of the file found it, or that the file holds none the reading can walk to
type serversRead struct {
	modified time.Time
	size     int64
	entries  []serverEntry
	found    bool
}

// Entries of the servers setting in the order the profile's file declares them, their names and options in the case written there; the
// settings' own reading where the file holds no table it can walk to
func (f *serversFile) read(cfg *config.Config) []serverEntry {
	path := filepath.Join(cfg.ProfileDir(cfg.Profile()), "config.yaml")
	st, err := os.Stat(path)
	if err != nil {
		return fromSettings(cfg)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	hit, ok := f.cache[path]
	if !ok || !hit.modified.Equal(st.ModTime()) || hit.size != st.Size() {
		hit = serversRead{modified: st.ModTime(), size: st.Size()}
		hit.entries, hit.found = parseServers(path)
		if f.cache == nil {
			f.cache = map[string]serversRead{}
		}
		f.cache[path] = hit
	}
	if !hit.found {
		return fromSettings(cfg)
	}
	return hit.entries
}

// Entries of the servers table of a settings file, false when the file holds none that reads
func parseServers(path string) ([]serverEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return nil, false
	}
	node := lookup(&doc, strings.Split(lspconfig.Servers, ".")...)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, false
	}
	var entries []serverEntry
	for i := 0; i+1 < len(node.Content); i += 2 {
		var raw map[string]any
		if node.Content[i+1].Decode(&raw) == nil && raw != nil {
			entries = append(entries, serverEntry{id: node.Content[i].Value, raw: raw})
		}
	}
	return entries, true
}

// Mapping node under the keys, matched without case as the settings match them
func lookup(node *yaml.Node, keys ...string) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, key := range keys {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if strings.EqualFold(node.Content[i].Value, key) {
				next = node.Content[i+1]
			}
		}
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

// Entries of the servers setting as the settings read them, by name; they keep names in lower case, so variable names are read upper case
func fromSettings(cfg *config.Config) []serverEntry {
	table, _ := cfg.Get(lspconfig.Servers).(map[string]any)
	var entries []serverEntry
	for _, id := range slices.Sorted(maps.Keys(table)) {
		raw, ok := table[id].(map[string]any)
		if !ok {
			continue
		}
		if env, ok := raw["env"].(map[string]any); ok {
			raw = maps.Clone(raw)
			upper := map[string]any{}
			for k, v := range env {
				upper[strings.ToUpper(k)] = v
			}
			raw["env"] = upper
		}
		entries = append(entries, serverEntry{id: id, raw: raw})
	}
	return entries
}
