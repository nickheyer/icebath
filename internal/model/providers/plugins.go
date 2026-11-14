package providers

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

type PluginDir struct {
	Name    string
	Path    string
	Home    string
	HomeKey string
}

type PluginLoader interface {
	LoadProviders(ctx context.Context, dir PluginDir) (profiles []Profile, found bool, err error)
}

const (
	stampTTL     = time.Second
	mtimeTick    = 2 * time.Second
	missLimit    = 512
	manifestKind = "model-provider"
)

type stamp struct {
	ok    bool
	mtime int64
}

func readStamps(homeDir string) [2]stamp {
	at := func(path string) stamp {
		info, err := os.Stat(path)
		if err != nil {
			return stamp{}
		}
		return stamp{ok: true, mtime: info.ModTime().UnixNano()}
	}
	return [2]stamp{at(home.Plugins.In(homeDir)), at(home.ModelProviderPlugins.In(homeDir))}
}

func stampsSettled(stamps [2]stamp) bool {
	now := time.Now().UnixNano()
	for _, s := range stamps {
		if !s.ok {
			continue
		}
		d := now - s.mtime
		if d < 0 {
			d = -d
		}
		if d < int64(mtimeTick) {
			return false
		}
	}
	return true
}

func pluginChildren(dir string) []os.DirEntry {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func hiddenPluginName(name string) bool {
	return strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

func providerPluginDirs(homeDir, homeKey string) []PluginDir {
	dirs := []PluginDir{}
	userDir := home.ModelProviderPlugins.In(homeDir)
	for _, child := range pluginChildren(userDir) {
		path := filepath.Join(userDir, child.Name())
		if isDir(path) && !hiddenPluginName(child.Name()) {
			dirs = append(dirs, PluginDir{Name: child.Name(), Path: path, Home: homeDir, HomeKey: homeKey})
		}
	}
	installed := home.Plugins.In(homeDir)
	for _, child := range pluginChildren(installed) {
		path := filepath.Join(installed, child.Name())
		if !isDir(path) || hiddenPluginName(child.Name()) || child.Name() == filepath.Base(home.ModelProviderPlugins.In(homeDir)) {
			continue
		}
		if declaresModelProviderKind(path) {
			dirs = append(dirs, PluginDir{Name: child.Name(), Path: path, Home: homeDir, HomeKey: homeKey})
		}
	}
	return dirs
}

func declaresModelProviderKind(pluginDir string) bool {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		manifest := filepath.Join(pluginDir, name)
		info, err := os.Stat(manifest)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(manifest)
		if err != nil {
			return false
		}
		text := strings.ToValidUTF8(string(raw), "�")
		var data any
		if yaml.Unmarshal([]byte(text), &data) == nil {
			if m, ok := data.(map[string]any); ok {
				kind, _ := m["kind"].(string)
				return strings.TrimSpace(kind) == manifestKind
			}
		}
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			stripped := strings.TrimSpace(line)
			if strings.HasPrefix(stripped, "#") || !strings.Contains(stripped, ":") {
				continue
			}
			key, value, _ := strings.Cut(stripped, ":")
			if strings.TrimSpace(key) == "kind" {
				return strings.Trim(strings.TrimSpace(value), "\"'") == manifestKind
			}
		}
		return false
	}
	return false
}
