package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Plugin another harness installed, as this one reads it
type Foreign struct {
	ID          string
	Entry       string
	Marketplace string
	Root        string
	Data        string
	Version     string
	Enabled     bool
	From        string
}

// Plugins installed by Claude Code under its home, with the enablement its settings files give them
func ClaudeInstalls(projectDir string) ([]Foreign, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".claude")
	root := os.Getenv("CLAUDE_CODE_PLUGIN_CACHE_DIR")
	if root == "" {
		root = filepath.Join(base, "plugins")
	}
	doc, err := readJSON[struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}](filepath.Join(root, installedFile))
	if err != nil || len(doc.Plugins) == 0 {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, file := range []string{filepath.Join(base, "settings.json"), filepath.Join(projectDir, ".claude", "settings.json"), filepath.Join(projectDir, ".claude", "settings.local.json")} {
		s, err := readJSON[struct {
			Enabled map[string]bool `json:"enabledPlugins"`
		}](file)
		if err != nil {
			continue
		}
		for k, v := range s.Enabled {
			enabled[k] = v
		}
	}
	ids := make([]string, 0, len(doc.Plugins))
	for id := range doc.Plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Foreign
	for _, id := range ids {
		var records []struct {
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		}
		raw := doc.Plugins[id]
		if err := json.Unmarshal(raw, &records); err != nil {
			var one struct {
				InstallPath string `json:"installPath"`
				Version     string `json:"version"`
			}
			if json.Unmarshal(raw, &one) != nil {
				continue
			}
			records = append(records, one)
		}
		for _, r := range records {
			if r.InstallPath == "" || !isDir(r.InstallPath) {
				continue
			}
			entry, market := SplitID(id)
			on, set := enabled[id]
			if !set {
				on = true
			}
			out = append(out, Foreign{ID: id, Entry: entry, Marketplace: market, Root: r.InstallPath, Data: filepath.Join(root, dataDir, DataID(id)), Version: r.Version, Enabled: on, From: FromClaude})
			break
		}
	}
	return out, nil
}

// Plugins installed by Codex under its cache, with the enablement its config gives them
func CodexInstalls(projectDir string) ([]Foreign, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".codex")
	cache := filepath.Join(base, "plugins", cacheDir)
	markets, err := os.ReadDir(cache)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	enabled := map[string]bool{}
	for _, file := range []string{filepath.Join(base, "config.toml"), filepath.Join(projectDir, ".codex", "config.toml")} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for id, on := range tomlPlugins(string(data)) {
			enabled[id] = on
		}
	}
	var out []Foreign
	for _, m := range markets {
		if !m.IsDir() {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cache, m.Name()))
		if err != nil {
			continue
		}
		for _, p := range plugins {
			if !p.IsDir() {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(cache, m.Name(), p.Name()))
			if err != nil {
				continue
			}
			var newest string
			var at int64
			for _, v := range versions {
				if !v.IsDir() {
					continue
				}
				info, err := v.Info()
				if err != nil {
					continue
				}
				if newest == "" || info.ModTime().UnixNano() > at {
					newest, at = v.Name(), info.ModTime().UnixNano()
				}
			}
			if newest == "" {
				continue
			}
			id := p.Name() + "@" + m.Name()
			on, set := enabled[id]
			if !set {
				on = true
			}
			out = append(out, Foreign{ID: id, Entry: p.Name(), Marketplace: m.Name(), Root: filepath.Join(cache, m.Name(), p.Name(), newest), Data: filepath.Join(base, "plugins", dataDir, DataID(id)), Version: newest, Enabled: on, From: FromCodex})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Enablement of plugins a Codex config holds under [plugins."name@marketplace"] tables
func tomlPlugins(text string) map[string]bool {
	out := map[string]bool{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			current = ""
			header := strings.Trim(line, "[]")
			if !strings.HasPrefix(header, "plugins.") {
				continue
			}
			rest := strings.TrimPrefix(header, "plugins.")
			if strings.HasPrefix(rest, "\"") {
				end := strings.Index(rest[1:], "\"")
				if end < 0 {
					continue
				}
				id := rest[1 : 1+end]
				if strings.TrimSpace(rest[1+end+1:]) != "" {
					continue
				}
				current = id
			} else if !strings.Contains(rest, ".") {
				current = rest
			}
			continue
		}
		if current == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "enabled" {
			continue
		}
		out[current] = strings.TrimSpace(value) == "true"
	}
	return out
}
