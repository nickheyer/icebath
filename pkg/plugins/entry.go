package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

var listingBundledSkip = map[string]bool{"memory": true, "context_engine": true, "computer_use": true, "model-providers": true, "config_backends": true}

// One plugin the loader sees with its display metadata, source label, directory and registry key
type PluginEntry struct {
	Name        string
	Version     string
	Description string
	Source      string
	Dir         string
	Key         string
}

// Entries keyed by registry key in first insertion order, later sources replacing earlier values in place
type entrySet struct {
	order []string
	byKey map[string]PluginEntry
}

func (s *entrySet) put(e PluginEntry) {
	if _, ok := s.byKey[e.Key]; !ok {
		s.order = append(s.order, e.Key)
	}
	s.byKey[e.Key] = e
}

// Display name, version, description and key of a native or portable plugin directory, ok false for none
func readManifestInfo(d, prefix string) (string, any, any, string, bool, error) {
	manifestFile, err := pm.NativeManifestFile(d)
	if err != nil {
		return "", nil, nil, "", false, &PluginOperationError{Msg: err.Error()}
	}
	var manifest map[string]any
	var name string
	if manifestFile == "" {
		portable := filepath.Join(d, "plugin.json")
		if _, err := os.Lstat(portable); err != nil {
			return "", nil, nil, "", false, nil
		}
		m, _, err := ReadAgentPluginManifest(d)
		if err != nil || m == nil {
			return "", nil, nil, "", false, nil
		}
		raw, ok := m["name"]
		if !ok {
			return "", nil, nil, "", false, nil
		}
		manifest, name = m, scalarString(raw)
	} else {
		if m, err := pm.ReadNativeManifest(manifestFile); err == nil && m != nil {
			manifest = m
		} else {
			manifest = map[string]any{}
		}
		name = filepath.Base(d)
		if raw, ok := manifest["name"]; ok {
			name = scalarString(raw)
		}
	}
	key := name
	if prefix != "" {
		key = prefix + "/" + filepath.Base(d)
	}
	version, ok := manifest["version"]
	if !ok {
		version = ""
	}
	description, ok := manifest["description"]
	if !ok {
		description = ""
	}
	return name, version, description, key, true, nil
}

// Recursive plugin directory scan matching the loader, category directories one level deep
func scanLevel(base, source string, skip map[string]bool, prefix string, depth int, seen *entrySet) {
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return
	}
	children, err := os.ReadDir(base)
	if err != nil {
		logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", base, pyErrorText(err)))
		return
	}
	for _, child := range children {
		d := filepath.Join(base, child.Name())
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() || (depth == 0 && len(skip) > 0 && skip[child.Name()]) {
			continue
		}
		name, version, description, key, ok, err := readManifestInfo(d, prefix)
		if err != nil {
			logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", d, pyErrorText(err)))
			continue
		}
		if !ok {
			if depth == 0 {
				sub := child.Name()
				if prefix != "" {
					sub = prefix + "/" + child.Name()
				}
				scanLevel(d, source, nil, sub, 1, seen)
			}
			continue
		}
		if _, dup := seen.byKey[key]; dup && source == "bundled" {
			continue
		}
		label := source
		if source == "user" {
			if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
				label = "git"
			}
		}
		seen.put(PluginEntry{Name: name, Version: scalarString(version), Description: scalarString(description), Source: label, Dir: d, Key: key})
	}
}

// Every plugin the loader sees in load order, compiled-in and bundled first, then user plugins, then entry points
func DiscoverAllPlugins(h string) ([]PluginEntry, error) {
	seen := &entrySet{byKey: map[string]PluginEntry{}}
	for _, m := range bundledManifests() {
		if _, dup := seen.byKey[m.Key]; !dup {
			seen.put(PluginEntry{Name: m.Name, Version: m.Version, Description: m.Description, Source: "bundled", Key: m.Key})
		}
	}
	if dir := GetBundledPluginsDir(); dir != "" {
		scanLevel(dir, "bundled", listingBundledSkip, "", 0, seen)
	}
	user, err := pluginsDir(h)
	if err != nil {
		return nil, err
	}
	scanLevel(user, "user", nil, "", 0, seen)
	for _, m := range discoverEntrypointManifests(context.Background()) {
		if _, dup := seen.byKey[m.Name]; !dup {
			seen.put(PluginEntry{Name: m.Name, Version: m.Version, Description: m.Description, Source: "entrypoint", Dir: m.Path, Key: m.Name})
		}
	}
	out := make([]PluginEntry, 0, len(seen.order))
	for _, key := range seen.order {
		out = append(out, seen.byKey[key])
	}
	return out, nil
}

// Install directory whose installer record names a catalog entry, empty when none or ambiguous
func catalogInstalledDir(h, name string) string {
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return ""
	}
	var hits []string
	for key := range metadata {
		target := filepath.Join(home.PluginsDir(h), key)
		if st, err := os.Stat(target); err != nil || !st.IsDir() {
			continue
		}
		if record := readCatalogSidecar(h, target); record != nil && record["catalog_name"] == name {
			hits = append(hits, target)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// Plugin entry for a key or manifest name, else a unique bare leaf, else the catalog name it was installed under
func FindPluginEntry(h, name string) (*PluginEntry, error) {
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	target := legacyPluginAlias(name)
	if target == "" {
		target = name
	}
	for i := range entries {
		if entries[i].Name == target || entries[i].Key == target {
			return &entries[i], nil
		}
	}
	var leaf []*PluginEntry
	for i := range entries {
		parts := strings.Split(entries[i].Key, "/")
		if parts[len(parts)-1] == name {
			leaf = append(leaf, &entries[i])
		}
	}
	if len(leaf) > 0 {
		if len(leaf) == 1 {
			return leaf[0], nil
		}
		return nil, nil
	}
	installed := catalogInstalledDir(h, name)
	if installed == "" {
		return nil, nil
	}
	for i := range entries {
		if entries[i].Dir != "" && filepath.Clean(entries[i].Dir) == filepath.Clean(installed) {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// Entry directory of an installed or bundled plugin under the process home, found false when unresolved
func FindPluginEntryDir(plugin string) (string, bool, error) {
	entry, err := FindPluginEntry(home.Dir(context.Background()), plugin)
	if err != nil || entry == nil {
		return "", false, err
	}
	return entry.Dir, true, nil
}
