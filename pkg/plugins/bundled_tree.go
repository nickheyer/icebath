package plugins

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugintree"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Bundled plugins tree a reader walks, the ICEBATH_BUNDLED_PLUGINS directory when set, else the copy compiled in
func BundledPluginsFS() fs.FS {
	if override := bundledOverrideDir(); override != "" {
		return os.DirFS(override)
	}
	return plugintree.Compiled()
}

// Directory ICEBATH_BUNDLED_PLUGINS names, empty when the compiled tree serves the bundled readers
func bundledOverrideDir() string { return os.Getenv("ICEBATH_BUNDLED_PLUGINS") }

// Display name, version, description and key of a compiled tree plugin directory, ok false when it holds no manifest
func readCompiledManifestInfo(tree fs.FS, d, prefix string) (string, any, any, string, bool, error) {
	manifestFile, err := pm.NativeManifestFileFS(tree, d)
	if err != nil {
		return "", nil, nil, "", false, &PluginOperationError{Msg: err.Error()}
	}
	if manifestFile == "" {
		return "", nil, nil, "", false, nil
	}
	manifest, err := pm.ReadNativeManifestFS(tree, manifestFile)
	if err != nil || manifest == nil {
		manifest = map[string]any{}
	}
	name := path.Base(d)
	if raw, ok := manifest["name"]; ok {
		name = scalarString(raw)
	}
	key := name
	if prefix != "" {
		key = prefix + "/" + path.Base(d)
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

// Listing scan of the compiled bundled tree matching scanLevel, category directories one level deep, entries without a directory
func scanCompiledLevel(tree fs.FS, base string, skip map[string]bool, prefix string, depth int, seen *entrySet) {
	children, err := fs.ReadDir(tree, base)
	if err != nil {
		logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", base, pyErrorText(err)))
		return
	}
	for _, child := range children {
		d := path.Join(base, child.Name())
		if !child.IsDir() || (depth == 0 && len(skip) > 0 && skip[child.Name()]) {
			continue
		}
		name, version, description, key, ok, err := readCompiledManifestInfo(tree, d, prefix)
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
				scanCompiledLevel(tree, d, nil, sub, 1, seen)
			}
			continue
		}
		if _, dup := seen.byKey[key]; dup {
			continue
		}
		seen.put(PluginEntry{Name: name, Version: scalarString(version), Description: scalarString(description), Source: "bundled", Key: key})
	}
}

// Manifest of the compiled tree plugin listed under a key, false under the override or for an unknown key
func compiledBundledManifest(key string) (map[string]any, bool) {
	if bundledOverrideDir() != "" {
		return nil, false
	}
	tree := plugintree.Compiled()
	var found map[string]any
	var walk func(base, prefix string, depth int, skip map[string]bool) bool
	walk = func(base, prefix string, depth int, skip map[string]bool) bool {
		children, _ := fs.ReadDir(tree, base)
		for _, child := range children {
			d := path.Join(base, child.Name())
			if !child.IsDir() || (depth == 0 && skip[child.Name()]) {
				continue
			}
			_, _, _, k, ok, err := readCompiledManifestInfo(tree, d, prefix)
			if err != nil {
				continue
			}
			if !ok {
				sub := child.Name()
				if prefix != "" {
					sub = prefix + "/" + child.Name()
				}
				if depth == 0 && walk(d, sub, 1, nil) {
					return true
				}
				continue
			}
			if k == key {
				manifestFile, _ := pm.NativeManifestFileFS(tree, d)
				if m, err := pm.ReadNativeManifestFS(tree, manifestFile); err == nil {
					found = m
				} else {
					found = map[string]any{}
				}
				return true
			}
		}
		return false
	}
	if !walk(".", "", 0, listingBundledSkip) {
		return nil, false
	}
	return found, true
}

// Whether a bundled manifest is active with no plugins.enabled entry, a model provider or a default on bundled kind
func bundledManifestDefaultOn(manifest map[string]any) bool {
	var rawKind any = "standalone"
	if v, ok := manifest["kind"]; ok {
		rawKind = v
	}
	kind := strings.ToLower(pystr.Strip(pyrepr.Str(rawKind)))
	return kind == "model-provider" || bundledDefaultOnKinds[kind]
}

// Tool names a compiled tree plugin directory declares in provides_tools, false under the override or without that directory
func compiledProvidesTools(name string) ([]string, bool) {
	if bundledOverrideDir() != "" || !fs.ValidPath(name) {
		return nil, false
	}
	tree := plugintree.Compiled()
	if st, err := fs.Stat(tree, name); err != nil || !st.IsDir() {
		return nil, false
	}
	manifest := map[string]any{}
	if manifestFile, err := pm.NativeManifestFileFS(tree, name); err == nil && manifestFile != "" {
		if m, err := pm.ReadNativeManifestFS(tree, manifestFile); err == nil {
			manifest = m
		}
	}
	provided, _ := manifest["provides_tools"].([]any)
	names := make([]string, 0, len(provided))
	for _, p := range provided {
		names = append(names, scalarString(p))
	}
	return names, true
}

// Plugins package root the host child execs parent packages from, the override directory when set, else the SDK plugins package
func hostPluginsRoot() (string, error) {
	if dir := bundledOverrideDir(); dir != "" {
		return dir, nil
	}
	root, err := materializeHostChild()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sdk", "plugins"), nil
}
