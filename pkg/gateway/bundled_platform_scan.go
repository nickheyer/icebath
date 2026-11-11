package gateway

import (
	"bytes"
	"io/fs"
	"path"
	"sort"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Lower cased manifest name of a bundled platform plugin directory of a tree, empty when absent or unreadable
func bundledPlatformManifestName(tree fs.FS, pluginDir string) string {
	var manifest string
	for _, m := range []string{"plugin.yaml", "plugin.yml"} {
		if _, err := fs.Stat(tree, path.Join(pluginDir, m)); err == nil {
			manifest = path.Join(pluginDir, m)
			break
		}
	}
	if manifest == "" {
		return ""
	}
	raw, err := fs.ReadFile(tree, manifest)
	if err != nil || !utf8.Valid(raw) {
		return ""
	}
	data, err := config.FastSafeLoad(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if err != nil {
		return ""
	}
	if !pyrepr.Truthy(data) {
		data = map[string]any{}
	}
	m, ok := data.(map[string]any)
	if !ok {
		return pystr.Lower(pystr.Strip(pyrepr.Str(nil)))
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(m["name"])))
}

// Bundled platform plugin directories holding a manifest, plus manifest names that differ from their directory
func scanBundledPluginPlatforms() (map[string]bool, map[string]string) {
	tree := plugins.BundledPluginsFS()
	entries, err := fs.ReadDir(tree, "platforms")
	if err != nil {
		return map[string]bool{}, map[string]string{}
	}
	var dirs []string
	for _, e := range entries {
		child := path.Join("platforms", e.Name())
		if fi, err := fs.Stat(tree, child); err != nil || !fi.IsDir() {
			continue
		}
		for _, m := range []string{"plugin.yaml", "plugin.yml"} {
			if _, err := fs.Stat(tree, path.Join(child, m)); err == nil {
				dirs = append(dirs, e.Name())
				break
			}
		}
	}
	names := map[string]bool{}
	for _, d := range dirs {
		names[pystr.Lower(d)] = true
	}
	aliases := map[string]string{}
	for _, d := range dirs {
		name := bundledPlatformManifestName(tree, path.Join("platforms", d))
		if _, taken := aliases[name]; name != "" && !names[name] && !taken {
			aliases[name] = pystr.Lower(d)
		}
	}
	return names, aliases
}

// Bundled platform directory names followed by their manifest aliases, sorted, read from the once per process scan
func bundledPlatformNamesAndAliases() []string {
	names, aliases := bundledPlatformScanCached()
	out := make([]string, 0, len(names)+len(aliases))
	for n := range names {
		out = append(out, n)
	}
	for a := range aliases {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
