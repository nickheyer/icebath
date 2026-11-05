package pm

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// PluginManifestHost is the plugin manifest surface pkg/plugins installs at init, a package pm cannot import
type PluginManifestHost struct {
	SupportedManifestVersion int
	RunningVersion           func() string
	RequiresError            func(manifest map[string]any) string
	ReadAgentManifest        func(pluginDir string) (map[string]any, error)
}

var pluginManifestHost *PluginManifestHost

// Installs the plugin manifest surface that declarations, admission and fit checks read
func SetPluginManifestHost(host *PluginManifestHost) { pluginManifestHost = host }

// Installed plugin manifest surface, the ImportError Python raises when its plugin modules are absent
func manifestHost() (*PluginManifestHost, error) {
	if pluginManifestHost == nil {
		return nil, &PyError{Class: "ImportError", Message: "cannot import name 'plugins_manifest' from 'icebath' (pkg/plugins is not linked into this binary)"}
	}
	return pluginManifestHost, nil
}

// Text of a file decoded as utf-8-sig, invalid bytes reported as Python's UnicodeDecodeError
func readUTF8SigText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if err := strictUTF8(data); err != nil {
		return "", &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	return string(data), nil
}

// Native plugin.yaml or plugin.yml of a plugin dir, empty when neither exists
func NativeManifestFile(pluginDir string) (string, error) {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		path := filepath.Join(pluginDir, name)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not inspect plugin manifest %s: %s", path, pyOSErrorText(err))}
		}
		return path, nil
	}
	return "", nil
}

// Mapping keys of a decoded YAML value made strings throughout, as Python dicts hold them
func yamlNormalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			t[k] = yamlNormalize(inner)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, inner := range t {
			out[fmt.Sprint(k)] = yamlNormalize(inner)
		}
		return out
	case []any:
		for i, inner := range t {
			t[i] = yamlNormalize(inner)
		}
		return t
	}
	return v
}

// Parsed native manifest, empty YAML an empty mapping and anything else but a mapping refused
func ReadNativeManifest(path string) (map[string]any, error) {
	text, err := readUTF8SigText(path)
	if err != nil {
		var pyErr *PyError
		message := pyOSErrorText(err)
		if errors.As(err, &pyErr) {
			message = pyErr.Message
		}
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not read plugin manifest %s: %s", path, message)}
	}
	var data any
	if err := yaml.Unmarshal([]byte(text), &data); err != nil {
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("Could not read plugin manifest %s: %s", path, err)}
	}
	if data == nil {
		return map[string]any{}, nil
	}
	manifest, ok := yamlNormalize(data).(map[string]any)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: "Plugin manifest must be a mapping: " + path}
	}
	return manifest, nil
}

// Python int() of a manifest scalar, ok false where int() raises TypeError or ValueError
func pyIntValue(v any) (*big.Int, bool, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewInt(1), true, nil
		}
		return big.NewInt(0), true, nil
	case int:
		return big.NewInt(int64(t)), true, nil
	case int64:
		return big.NewInt(t), true, nil
	case uint64:
		return new(big.Int).SetUint64(t), true, nil
	case float64:
		if math.IsInf(t, 0) {
			return nil, false, &PyError{Class: "OverflowError", Message: "cannot convert float infinity to integer"}
		}
		if math.IsNaN(t) {
			return nil, false, nil
		}
		n, _ := big.NewFloat(math.Trunc(t)).Int(nil)
		return n, true, nil
	case string:
		return pyIntString(t)
	}
	return nil, false, nil
}

// Python int() of a string, signed decimal digits of any script with single underscores between digits
func pyIntString(text string) (*big.Int, bool, error) {
	text = pyStrip(text)
	sign := ""
	if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		sign, text = text[:1], text[1:]
	}
	var digits strings.Builder
	previousUnderscore := true
	for _, r := range text {
		if r == '_' {
			if previousUnderscore {
				return nil, false, nil
			}
			previousUnderscore = true
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return nil, false, nil
		}
		start := r
		for unicode.Is(unicode.Nd, start-1) {
			start--
		}
		digits.WriteByte(byte('0' + (r-start)%10))
		previousUnderscore = false
	}
	if digits.Len() == 0 || previousUnderscore {
		return nil, false, nil
	}
	n, ok := new(big.Int).SetString(sign+digits.String(), 10)
	return n, ok, nil
}

// Why a manifest cannot load here, its required icebath version or a manifest_version newer than supported
func ManifestVersionError(manifest map[string]any, name string) (string, error) {
	host, err := manifestHost()
	if err != nil {
		return "", err
	}
	if reason := host.RequiresError(manifest); reason != "" {
		return fmt.Sprintf("Plugin '%s' %s", name, reason), nil
	}
	version, ok := manifest["manifest_version"]
	if !ok || version == nil {
		return "", nil
	}
	parsed, valid, err := pyIntValue(version)
	if err != nil {
		return "", err
	}
	if !valid {
		return fmt.Sprintf("Plugin '%s' has invalid manifest_version '%s' (expected an integer).", name, pyStr(version)), nil
	}
	if parsed.Cmp(big.NewInt(int64(host.SupportedManifestVersion))) > 0 {
		return fmt.Sprintf("Plugin '%s' requires manifest_version %s, but this installer only supports up to %d.", name, pyStr(version), host.SupportedManifestVersion), nil
	}
	return "", nil
}

// PythonDeclaration is a plugin's effective Python surface for consent, membership, builds and currency
type PythonDeclaration struct {
	Files             []string
	Pyproject         string
	Requirements      []string
	Manifest          map[string]any
	RequiresPython    *string
	PyprojectInstalls bool
}

// Declaration whose requirements are validated unless the plugin brings its own Python runtime
func newPythonDeclaration(d PythonDeclaration) (*PythonDeclaration, error) {
	if !d.External() {
		if _, err := ApplicableRequirements(d.Requirements); err != nil {
			return nil, err
		}
	}
	return &d, nil
}

// Whether the manifest declares an external python_runtime
func (d *PythonDeclaration) External() bool {
	value := d.Manifest["python_runtime"]
	text := ""
	if pyTruthy(value) {
		text = pyStr(value)
	}
	return strings.ToLower(pyStrip(text)) == "external"
}

// Requirements the shared environment installs, none for an external runtime
func (d *PythonDeclaration) InstallRequirements() ([]string, error) {
	if d.External() {
		return []string{}, nil
	}
	return ApplicableRequirements(d.Requirements)
}

// Whether admission would put packages into the shared environment, which makes the plugin a workspace member
func (d *PythonDeclaration) InstallsPackages() (bool, error) {
	if d.External() {
		return false, nil
	}
	install, err := d.InstallRequirements()
	if err != nil {
		return false, err
	}
	return len(install) > 0 || d.PyprojectInstalls, nil
}

// Why this member cannot join a workspace built for a Python version, empty when it fits
func (d *PythonDeclaration) PythonError(version string) (string, error) {
	installs, err := d.InstallsPackages()
	if err != nil || !installs || d.RequiresPython == nil || *d.RequiresPython == "" {
		return "", err
	}
	specs, err := parseSpecifierSet(*d.RequiresPython)
	if err != nil {
		return "", &PyError{Class: "InvalidSpecifier", Message: fmt.Sprintf("Invalid specifier: %s", pyRepr(*d.RequiresPython))}
	}
	ok, err := specifierSetContains(specs, version)
	if err != nil || ok {
		return "", err
	}
	return fmt.Sprintf("requires Python %s, but icebath runs on Python %s", *d.RequiresPython, version), nil
}

// Index requirements only, direct URLs and the core project itself dropped with markers kept
func ApplicableRequirements(specs []string) ([]string, error) {
	out := []string{}
	for _, spec := range specs {
		req, err := ParseRequirement(spec)
		if err != nil {
			return nil, err
		}
		if req.URL == "" && CanonicalizeName(req.Name) != "icebath-agent" {
			out = append(out, spec)
		}
	}
	return out, nil
}

// Requirements that name a direct URL, which the shared environment refuses
func UnsupportedRequirements(specs []string) ([]string, error) {
	out := []string{}
	for _, spec := range specs {
		req, err := ParseRequirement(spec)
		if err != nil {
			return nil, err
		}
		if req.URL != "" {
			out = append(out, spec)
		}
	}
	return out, nil
}

// Whether a decoded TOML value is a list of strings
func tomlStringList(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// Whether any value of a decoded table is truthy, the AttributeError Python raises for a non table
func tomlAnyValue(v any) (bool, error) {
	obj, ok := v.(*OrderedObject)
	if !ok {
		return false, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'values'", tomlTypeName(v))}
	}
	for _, k := range obj.Keys {
		if tomlTruthy(obj.Values[k]) {
			return true, nil
		}
	}
	return false, nil
}

// Pyproject install declaration when it has a project table, nil when the manifest declaration applies
func pyprojectDeclaration(project, text string, files []string, manifest map[string]any) (*PythonDeclaration, error) {
	document, err := tomlLoads(text)
	if err != nil {
		return nil, err
	}
	metadata, ok := document.Values["project"].(*OrderedObject)
	if !ok {
		return nil, nil
	}
	specsValue, present := metadata.Get("dependencies")
	if !present {
		specsValue = []any{}
	}
	specs, ok := tomlStringList(specsValue)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: "invalid project.dependencies: " + project}
	}
	var requiresPython *string
	if value, present := metadata.Get("requires-python"); present {
		s, ok := value.(string)
		if !ok {
			return nil, &PyError{Class: "ValueError", Message: "invalid project.requires-python: " + project}
		}
		requiresPython = &s
	}
	tool, err := tomlGet(document, "tool", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	uv, err := tomlGet(tool, "uv", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	_, installs := document.Get("build-system")
	if !installs {
		pkg, err := tomlGet(uv, "package", nil)
		if err != nil {
			return nil, err
		}
		installs = pkg == true
	}
	if !installs {
		optional, err := tomlGet(metadata, "optional-dependencies", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		if installs, err = tomlAnyValue(optional); err != nil {
			return nil, err
		}
	}
	if !installs {
		groups, err := tomlGet(document, "dependency-groups", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		if installs, err = tomlAnyValue(groups); err != nil {
			return nil, err
		}
	}
	if !installs {
		dev, err := tomlGet(uv, "dev-dependencies", nil)
		if err != nil {
			return nil, err
		}
		installs = dev != nil && tomlTruthy(dev)
	}
	return newPythonDeclaration(PythonDeclaration{Files: files, Pyproject: project, Requirements: specs, Manifest: manifest, RequiresPython: requiresPython, PyprojectInstalls: installs})
}

// One effective declaration of a plugin, a packaging pyproject owning it and the manifest lists otherwise
func ReadPythonDeclaration(pluginDir string) (*PythonDeclaration, error) {
	native, err := NativeManifestFile(pluginDir)
	if err != nil {
		return nil, err
	}
	var files []string
	manifest := map[string]any{}
	if native != "" {
		files = append(files, native)
		if manifest, err = ReadNativeManifest(native); err != nil {
			return nil, err
		}
	} else {
		portable := filepath.Join(pluginDir, "plugin.json")
		if _, err := os.Lstat(portable); err == nil {
			host, err := manifestHost()
			if err != nil {
				return nil, err
			}
			if manifest, err = host.ReadAgentManifest(pluginDir); err != nil {
				return nil, err
			}
			files = append(files, portable)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	project := filepath.Join(pluginDir, "pyproject.toml")
	text, err := readUTF8SigText(project)
	switch {
	case err == nil:
		files = append(files, project)
		if !strings.Contains(text, "GENERATED by pm") {
			declaration, err := pyprojectDeclaration(project, text, files, manifest)
			if err != nil || declaration != nil {
				return declaration, err
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	var specs []string
	seen := map[string]bool{}
	for _, key := range []string{"pip_dependencies", "python_dependencies"} {
		value, present := manifest[key]
		if !present {
			value = []any{}
		}
		values, ok := value.([]any)
		if ok {
			for _, v := range values {
				if _, isString := v.(string); !isString {
					ok = false
				}
			}
		}
		if !ok {
			where := native
			if where == "" {
				where = "None"
			}
			return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("invalid %s: %s", key, where)}
		}
		for _, v := range values {
			if s := v.(string); !seen[s] {
				seen[s] = true
				specs = append(specs, s)
			}
		}
	}
	if specs == nil {
		specs = []string{}
	}
	return newPythonDeclaration(PythonDeclaration{Files: files, Requirements: specs, Manifest: manifest})
}
