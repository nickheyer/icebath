package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/nickheyer/icebath/pkg/home"
)

// Platform manifest sources a scan reads, the bundled tree, the home's user plugins or both
const (
	PlatformSourceAll     = "all"
	PlatformSourceBundled = "bundled"
	PlatformSourceUser    = "user"
)

// Env name suffixes that make a platform variable a secret unless its declaration says otherwise
var PlatformSecretEnvSuffixes = []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_JSON"}

// Reports whether a directory name can hold a plugin, every name except a dunder cache dir
func isPluginDirName(name string) bool {
	return !(strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__"))
}

type platformManifestPath struct {
	dirName     string
	path        string
	requireKind bool
	info        os.FileInfo
	err         error
	fsys        fs.FS
}

// Bundled platform plugin root under the ICEBATH_BUNDLED_PLUGINS tree, empty when the compiled tree serves instead
func bundledPlatformRoot() string {
	dir := strings.TrimSpace(os.Getenv("ICEBATH_BUNDLED_PLUGINS"))
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "platforms")
}

// Every platform plugin manifest of a source with its stat, an unsearchable directory carries its error instead
func platformManifestPaths(h, source string) []platformManifestPath {
	type root struct {
		dir         string
		requireKind bool
	}
	var roots []root
	var out []platformManifestPath
	if source == PlatformSourceAll || source == PlatformSourceBundled {
		if dir := bundledPlatformRoot(); dir != "" {
			roots = append(roots, root{dir, false})
		} else {
			out = compiledPlatformManifestPaths()
		}
	}
	if source == PlatformSourceAll || source == PlatformSourceUser {
		userPlugins := home.PluginsDir(h)
		roots = append(roots, root{filepath.Join(userPlugins, "platforms"), false}, root{userPlugins, true})
	}
	for _, r := range roots {
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || pyOSErrorName(err) == "NotADirectoryError" {
				continue
			}
			if fi, serr := os.Stat(r.dir); serr == nil && !fi.IsDir() {
				continue
			}
			out = append(out, platformManifestPath{dirName: r.dir, requireKind: r.requireKind, err: err})
			continue
		}
		for _, entry := range entries {
			if !isPluginDirName(entry.Name()) {
				continue
			}
			dir := filepath.Join(r.dir, entry.Name())
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				continue
			}
			for _, name := range []string{"plugin.yaml", "plugin.yml"} {
				path := filepath.Join(dir, name)
				fi, err := os.Stat(path)
				if errors.Is(err, fs.ErrPermission) {
					out = append(out, platformManifestPath{dirName: entry.Name(), requireKind: r.requireKind, err: err})
					break
				}
				if err != nil || !fi.Mode().IsRegular() {
					continue
				}
				out = append(out, platformManifestPath{dirName: entry.Name(), path: path, requireKind: r.requireKind, info: fi})
				break
			}
		}
	}
	return out
}

// Change detection key over every user platform plugin manifest of a home, changing on any add, removal or edit
func PlatformManifestStamp(h string) string {
	var b strings.Builder
	for _, p := range platformManifestPaths(h, PlatformSourceUser) {
		if p.path == "" {
			fmt.Fprintf(&b, "%q\x00\x00%s\n", p.dirName, pyOSErrorName(p.err))
			continue
		}
		fmt.Fprintf(&b, "%q\x00%q\x00%v\n", p.dirName, p.path, FileSignature(p.info))
	}
	return b.String()
}

// Platform plugin manifest with the directory it was found in
type NamedPlatformManifest struct {
	DirName  string
	Manifest map[string]any
}

// Readable platform manifests of a source, strict refusing an unreadable platform manifest, every skip appended to skipped
func platformPluginManifests(h, source string, strict bool, skipped *[]string) ([]NamedPlatformManifest, error) {
	if SafeWorkerEnabled() {
		return nil, nil
	}
	var out []NamedPlatformManifest
	for _, p := range platformManifestPaths(h, source) {
		if p.path == "" {
			slog.Warn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", p.dirName, pyOSErrorString(p.err)))
			if skipped != nil {
				*skipped = append(*skipped, p.dirName)
			}
			continue
		}
		raw, err := p.read()
		if err != nil {
			if strict && !p.requireKind {
				return out, err
			}
			slog.Warn(fmt.Sprintf("Skipping unreadable plugin manifest %s: %s", p.path, pyOSErrorString(err)))
			if skipped != nil {
				*skipped = append(*skipped, p.path)
			}
			continue
		}
		raw = trimUTF8BOM(raw)
		var doc any
		if err = pyUTF8DecodeError(raw); err == nil {
			doc, err = FastSafeLoad(raw)
		}
		if err != nil {
			if skipped != nil {
				*skipped = append(*skipped, p.path)
			}
			continue
		}
		if !PyTruthy(doc) {
			doc = map[string]any{}
		}
		manifest, ok := doc.(map[string]any)
		if !ok || (p.requireKind && !pyEq(manifest["kind"], "platform")) {
			continue
		}
		out = append(out, NamedPlatformManifest{DirName: p.dirName, Manifest: manifest})
	}
	return out, nil
}

type platformEnvEntry struct {
	name     string
	isSecret bool
	meta     map[string]any
}

// Items a Python star unpack yields for a truthy value, list items, string characters or mapping keys
func pyStarItems(v any) ([]any, error) {
	if !PyTruthy(v) {
		return nil, nil
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(x))
		for _, k := range MappingKeys(x) {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("Value after * must be an iterable, not %s", pyTypeName(v))
}

// Python type name of a decoded YAML value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int64, uint64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Declared requires_env entries plus optional_env unless optional is false, a suffix named var a secret by default
func platformManifestEnvEntries(manifest map[string]any, optional bool) ([]platformEnvEntry, error) {
	entries, err := pyStarItems(manifest["requires_env"])
	if err != nil {
		return nil, err
	}
	if optional {
		extra, err := pyStarItems(manifest["optional_env"])
		if err != nil {
			return nil, err
		}
		entries = append(append([]any{}, entries...), extra...)
	}
	var out []platformEnvEntry
	for _, entry := range entries {
		var meta map[string]any
		switch x := entry.(type) {
		case string:
			meta = map[string]any{"name": x}
		case map[string]any:
			meta = x
		default:
			meta = map[string]any{}
		}
		name, ok := meta["name"].(string)
		if !ok || name == "" {
			continue
		}
		isSecret := PyTruthy(meta["password"]) || PyTruthy(meta["secret"])
		if pw, isBool := meta["password"].(bool); !isSecret && !(isBool && !pw) {
			upper := pyUpper(name)
			for _, suffix := range PlatformSecretEnvSuffixes {
				if strings.HasSuffix(upper, suffix) {
					isSecret = true
					break
				}
			}
		}
		out = append(out, platformEnvEntry{name: name, isSecret: isSecret, meta: meta})
	}
	return out, nil
}

// Python str.upper, the full Unicode case mapping that expands characters such as the sharp s
func pyUpper(s string) string {
	return cases.Upper(language.Und).String(s)
}

// Errors when the provider bridge has not registered the core declared env names
var ErrCoreEnvNamesUnregistered = errors.New("config: the provider bridge must register CoreDeclaredEnvNames before platform secrets are listed")

// Every env name core declares, the static table plus provider profile names, from the provider bridge
func CoreDeclaredEnvNames() (map[string]bool, error) {
	fn := hooks().CoreDeclaredEnvNames
	if fn == nil {
		return nil, ErrCoreEnvNamesUnregistered
	}
	return fn(), nil
}

// Upper cased messaging secret names the manifests declare minus the core names, nil core meaning every declared name
func manifestSecretEnvs(manifests []NamedPlatformManifest, core map[string]bool) (map[string]bool, error) {
	names := map[string]bool{}
	for _, m := range manifests {
		entries, err := platformManifestEnvEntries(m.Manifest, true)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			category := e.meta["category"]
			if !PyTruthy(category) {
				category = "messaging"
			}
			if e.isSecret && pyEq(category, "messaging") {
				names[pyUpper(e.name)] = true
			}
		}
	}
	if core == nil {
		var err error
		if core, err = CoreDeclaredEnvNames(); err != nil {
			return nil, err
		}
	}
	for n := range core {
		delete(names, pyUpper(n))
	}
	return names, nil
}

// Secret env names one source's platform manifests declare, the bundled set read once at startup minus core names
func PlatformManifestSecretEnvs(h, source string, strict bool) (map[string]bool, error) {
	if source == PlatformSourceBundled && bundledPlatformSecretEnvs != nil {
		core, err := CoreDeclaredEnvNames()
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(bundledPlatformSecretEnvs))
		for n := range bundledPlatformSecretEnvs {
			out[n] = true
		}
		for n := range core {
			delete(out, pyUpper(n))
		}
		return out, nil
	}
	manifests, err := platformPluginManifests(h, source, strict, nil)
	if err != nil {
		return nil, err
	}
	return manifestSecretEnvs(manifests, nil)
}

// A home's user platform plugin secrets read strictly and whether every plugin dir and manifest was read
func PlatformManifestSecretScan(h string) (map[string]bool, bool, error) {
	var skipped []string
	manifests, err := platformPluginManifests(h, PlatformSourceUser, true, &skipped)
	if err != nil {
		return nil, false, err
	}
	names, err := manifestSecretEnvs(manifests, nil)
	if err != nil {
		return nil, false, err
	}
	return names, len(skipped) == 0, nil
}

// Adds every platform manifest env entry to OptionalEnvVars and returns the bundled secrets, nil when a bundled manifest was unreadable
func injectPlatformPluginEnvVars(h string) (map[string]bool, error) {
	bundled, err := platformPluginManifests(h, PlatformSourceBundled, true, nil)
	bundledOK := err == nil
	manifests := append([]NamedPlatformManifest{}, bundled...)
	if !bundledOK {
		manifests = nil
	}
	sources := []string{PlatformSourceUser}
	if !bundledOK {
		sources = []string{PlatformSourceBundled, PlatformSourceUser}
	}
	for _, source := range sources {
		more, err := platformPluginManifests(h, source, false, nil)
		if err == nil {
			manifests = append(manifests, more...)
		}
	}
	for _, m := range manifests {
		label := m.Manifest["label"]
		if !PyTruthy(label) {
			label = m.Manifest["name"]
		}
		if !PyTruthy(label) {
			label = m.DirName
		}
		entries, err := platformManifestEnvEntries(m.Manifest, true)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if OptionalEnvVars.Has(e.name) {
				continue
			}
			category := "messaging"
			if PyTruthy(e.meta["category"]) {
				category = PyStr(e.meta["category"])
			}
			OptionalEnvVars.Set(e.name, EnvVar{
				Description: firstNonEmpty(pyOrEmpty(e.meta["description"]), PyStr(label)+" configuration"),
				Prompt:      firstNonEmpty(pyOrEmpty(e.meta["prompt"]), e.name),
				URL:         pyOrEmpty(e.meta["url"]),
				Password:    e.isSecret,
				Category:    category,
			})
		}
	}
	if !bundledOK {
		return nil, nil
	}
	return manifestSecretEnvs(bundled, staticCoreEnvNames)
}

// Names core declares before any platform manifest is read, a manifest never reclassifies one
var staticCoreEnvNames = func() map[string]bool {
	out := map[string]bool{}
	for _, name := range OptionalEnvVars.RawNames() {
		out[name] = true
	}
	return out
}()

// Bundled platform manifests' secret names read at startup, nil when a bundled manifest could not be read
var bundledPlatformSecretEnvs = func() map[string]bool {
	names, err := injectPlatformPluginEnvVars(home.Process())
	if err != nil {
		panic(fmt.Sprintf("config: platform plugin env declarations: %v", err))
	}
	return names
}()

// Copy of the names core declared before platform manifests were read
func StaticCoreEnvNames() map[string]bool {
	out := make(map[string]bool, len(staticCoreEnvNames))
	for n := range staticCoreEnvNames {
		out[n] = true
	}
	return out
}

// Bundled platform plugins with their manifest name and required env, the shape config check reads
func BundledPlatformManifests(h string) ([]PlatformManifest, error) {
	manifests, err := platformPluginManifests(h, PlatformSourceBundled, false, nil)
	if err != nil {
		return nil, err
	}
	out := make([]PlatformManifest, 0, len(manifests))
	for _, m := range manifests {
		entries, err := platformManifestEnvEntries(m.Manifest, false)
		if err != nil {
			return nil, err
		}
		required := make([]string, 0, len(entries))
		for _, e := range entries {
			required = append(required, e.name)
		}
		out = append(out, PlatformManifest{Name: m.DirName, ManifestName: PyStr(m.Manifest["name"]), RequiredEnv: required})
	}
	return out, nil
}
