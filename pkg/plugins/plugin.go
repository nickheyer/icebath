package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// A plugin compiled into the binary that registers its tools, hooks and commands through a context
type Plugin interface {
	Manifest() Manifest
	Register(ctx *Context) error
}

// Error raised by plugin management operations with a user facing message and its failure class
type PluginOperationError struct {
	Msg          string
	FailureClass string
}

func (e *PluginOperationError) Error() string { return e.Msg }

var (
	bundledMu    sync.RWMutex
	bundledByKey = map[string]Plugin{}
	bundledOrder []string
)

// Adds a compiled-in plugin to the bundled set, panicking on an empty or duplicate registry key
func RegisterBundled(p Plugin) {
	m := p.Manifest()
	key := m.Key
	if key == "" {
		key = m.Name
	}
	if key == "" {
		panic("plugins: RegisterBundled with an empty plugin name")
	}
	bundledMu.Lock()
	defer bundledMu.Unlock()
	if _, dup := bundledByKey[key]; dup {
		panic("plugins: bundled plugin " + key + " registered twice")
	}
	bundledByKey[key] = p
	bundledOrder = append(bundledOrder, key)
}

// Registry keys of every compiled-in plugin in registration order
func BundledPluginKeys() []string {
	bundledMu.RLock()
	defer bundledMu.RUnlock()
	return append([]string(nil), bundledOrder...)
}

// Manifests of every compiled-in plugin in registration order, tagged bundled with no directory
func bundledManifests() []*Manifest {
	bundledMu.RLock()
	defer bundledMu.RUnlock()
	out := make([]*Manifest, 0, len(bundledOrder))
	for _, key := range bundledOrder {
		m := bundledByKey[key].Manifest()
		m.Source = "bundled"
		m.Path = ""
		m.Key = key
		if m.Name == "" {
			m.Name = key
		}
		if m.Kind == "" {
			m.Kind = "standalone"
		}
		if m.ManifestVersion == 0 {
			m.ManifestVersion = 1
		}
		out = append(out, &m)
	}
	return out
}

// Compiled-in plugin registered under a key
func bundledPlugin(key string) (Plugin, bool) {
	bundledMu.RLock()
	defer bundledMu.RUnlock()
	p, ok := bundledByKey[key]
	return p, ok
}

// Profile-local plugin install metadata keyed by plugin directory name
func readInstallMetadata(h string) (map[string]any, error) {
	path := filepath.Join(home.PluginsDir(h), ".install-metadata.json")
	if _, err := os.Stat(path); err != nil {
		return map[string]any{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Could not read plugin install metadata: %v", err)}
	}
	if !utf8.Valid(stripBOM(raw)) {
		return nil, fmt.Errorf("'utf-8' codec can't decode %s", path)
	}
	value, err := pyjson.Loads(string(stripBOM(raw)))
	if err != nil {
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Could not read plugin install metadata: %v", err)}
	}
	doc, ok := pyjson.Plain(value).(map[string]any)
	if !ok {
		return nil, &PluginOperationError{Msg: "Plugin install metadata must be a JSON object."}
	}
	return doc, nil
}

// Profile-local plugin install metadata keyed by plugin directory name, for callers outside the package
func ReadInstallMetadata(h string) (map[string]any, error) { return readInstallMetadata(h) }

// Installer record of a directory directly under the plugins dir, nil otherwise
func installRecord(h, pluginDir string) map[string]any {
	if filepath.Dir(filepath.Clean(pluginDir)) != filepath.Clean(home.PluginsDir(h)) {
		return nil
	}
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return nil
	}
	record, _ := metadata[filepath.Base(pluginDir)].(map[string]any)
	return record
}

// Kill list verdict for an installed plugin by name, recorded source, catalog block or directory
func InstalledPluginRemoval(h, name, pluginDir string) *RemovedEntry {
	var record map[string]any
	if pluginDir != "" {
		record = installRecord(h, pluginDir)
	}
	if allow, ok := record["allow_removed"].(bool); ok && allow {
		return nil
	}
	block, _ := record["catalog"].(map[string]any)
	source, _, _ := strings.Cut(orEmpty(record["source"]), "#")
	candidates := []any{name, source, block["name"], block["repo"]}
	if pluginDir != "" {
		candidates = append(candidates, filepath.Base(pluginDir))
	}
	entries := cachedRemovedEntries(h)
	for _, candidate := range candidates {
		if !jsonTruthy(candidate) {
			continue
		}
		if removed := matchRemoved(scalarString(candidate), entries); removed != nil {
			return removed
		}
	}
	return nil
}
