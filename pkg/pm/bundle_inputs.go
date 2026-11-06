package pm

import (
	"fmt"

	"github.com/dlclark/regexp2/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bundled resource directories and the environment variable each one is published under, in order
var resourceEnv = [][2]string{
	{"skills", "ICEBATH_BUNDLED_SKILLS"},
	{"optional-skills", "ICEBATH_OPTIONAL_SKILLS"},
	{"plugins", "ICEBATH_BUNDLED_PLUGINS"},
	{"locales", "ICEBATH_BUNDLED_LOCALES"},
	{"optional-mcps", "ICEBATH_OPTIONAL_MCPS"},
}

// Bundled resource directory names in table order
func resourceEnvNames() []string {
	names := make([]string, len(resourceEnv))
	for i, row := range resourceEnv {
		names[i] = row[0]
	}
	return names
}

// Environment variable a bundled resource directory is published under, empty for an unknown name
func resourceEnvVar(name string) string {
	for _, row := range resourceEnv {
		if row[0] == name {
			return row[1]
		}
	}
	return ""
}

// Frontend product names in the order a payload plants them
var frontendNames = []string{"tui", "web", "webapp"}

// Keys of m ordered by their index in order, unknown keys after in sorted order
func orderedKeys(m map[string]string, order []string) []string {
	keys := make([]string, 0, len(m))
	for _, name := range order {
		if _, ok := m[name]; ok {
			keys = append(keys, name)
		}
	}
	var rest []string
	for name := range m {
		if !slices.Contains(order, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// Site packages directory of an environment for the target's Python pin, not the builder's
func dependencySite(venvDir, pythonVersion, target string) (string, error) {
	version, _, _ := strings.Cut(pythonVersion, "+")
	minor := version
	if i := strings.LastIndex(version, "."); i >= 0 {
		minor = version[:i]
	}
	if strings.HasPrefix(target, "win32") {
		return filepath.Join(venvDir, "Lib", "site-packages"), nil
	}
	return filepath.Join(venvDir, "lib", "python"+minor, "site-packages"), nil
}

var (
	commandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	scriptEntryPattern = mustCompilePython(`\A(?:[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*:[A-Za-z_]\w*)\Z`)
	targetPattern      = regexp.MustCompile(`^(?:(?:linux|darwin|win32)-(?:x64|arm64)|linux-(?:x64|arm64)-musl|linux-arm64-bionic)$`)
	binDirPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Python re pattern compiled for a package level constant, panicking on a pattern re rejects
func mustCompilePython(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Project table of a pyproject file read as utf-8-sig TOML, KeyError when it has none
func loadProjectTable(project string) (*OrderedObject, error) {
	raw, err := os.ReadFile(project)
	if err != nil {
		return nil, err
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	doc, err := tomlLoads(text)
	if err != nil {
		return nil, err
	}
	value, ok := doc.Get("project")
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr("project")}
	}
	table, ok := value.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("%s object is not subscriptable", pyrepr.StrRepr(tomlTypeName(value)))}
	}
	return table, nil
}

// Declared console scripts of a pyproject, refusing invalid command names and entry references
func projectEntries(project string) (*OrderedObject, error) {
	table, err := loadProjectTable(project)
	if err != nil {
		return nil, err
	}
	value, ok := table.Get("scripts")
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr("scripts")}
	}
	entries, ok := value.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'items'", pyrepr.StrRepr(tomlTypeName(value)))}
	}
	for _, name := range entries.Keys {
		if !commandNamePattern.MatchString(name) {
			return nil, &PyError{Class: "ValueError", Message: "invalid command name: " + name}
		}
		entry, ok := entries.Values[name].(string)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got %s", pyrepr.StrRepr(tomlTypeName(entries.Values[name])))}
		}
		if matched, _ := scriptEntryPattern.MatchString(entry); !matched {
			return nil, &PyError{Class: "ValueError", Message: "invalid script entry: " + entry}
		}
	}
	return entries, nil
}

// Build time paths of an agent assembly, an empty path or nil pointer standing for None
type AgentInputs struct {
	Project, Code, Repo, Placement, Target, Python, SitePackages, Environment, Tools, PMRuntime string
	BinDir                                                                                      *string
	CommandDir                                                                                  string
	Resources                                                                                   map[string]string
	Frontends                                                                                   map[string]string
	Ref                                                                                         *string
	Stamp                                                                                       string
	Features                                                                                    string
	Env                                                                                         *pyjson.Object
}

// Launcher directory relative to the output, bin when unset
func (inputs AgentInputs) binDir() string {
	if inputs.BinDir == nil {
		return "bin"
	}
	return *inputs.BinDir
}

// Field names of the inputs record in declaration order
var agentInputFields = []string{"project", "code", "repo", "placement", "target", "python", "site_packages",
	"environment", "pm_runtime", "bin_dir", "tools", "command_dir", "resources", "frontends", "ref", "stamp",
	"features", "env"}

// Ordered record of the inputs the way dataclasses.asdict renders them with str paths
func (inputs AgentInputs) asDict() *pyjson.Object {
	optional := func(path string) any {
		if path == "" {
			return nil
		}
		return path
	}
	paths := func(m map[string]string, order []string) *pyjson.Object {
		obj := pyjson.NewObject()
		for _, name := range orderedKeys(m, order) {
			obj.Set(name, m[name])
		}
		return obj
	}
	data := pyjson.NewObject()
	data.Set("project", inputs.Project)
	data.Set("code", inputs.Code)
	data.Set("repo", inputs.Repo)
	data.Set("placement", inputs.Placement)
	data.Set("target", inputs.Target)
	data.Set("python", inputs.Python)
	data.Set("site_packages", inputs.SitePackages)
	data.Set("environment", inputs.Environment)
	data.Set("pm_runtime", inputs.PMRuntime)
	data.Set("bin_dir", inputs.binDir())
	data.Set("tools", optional(inputs.Tools))
	data.Set("command_dir", optional(inputs.CommandDir))
	data.Set("resources", paths(inputs.Resources, resourceEnvNames()))
	data.Set("frontends", paths(inputs.Frontends, frontendNames))
	if inputs.Ref == nil {
		data.Set("ref", nil)
	} else {
		data.Set("ref", *inputs.Ref)
	}
	data.Set("stamp", optional(inputs.Stamp))
	data.Set("features", optional(inputs.Features))
	env := pyjson.NewObject()
	if inputs.Env != nil {
		for _, key := range inputs.Env.Keys {
			env.Set(key, inputs.Env.Vals[key])
		}
	}
	data.Set("env", env)
	return data
}

// Inputs from their recorded mapping, refusing unknown fields and missing required ones
func agentInputsFromDict(value any) (AgentInputs, error) {
	data, ok := value.(*pyjson.Object)
	if !ok {
		return AgentInputs{}, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'keys'", pyrepr.StrRepr(pyrepr.TypeName(value)))}
	}
	var unknown []string
	for _, key := range data.Keys {
		if !slices.Contains(agentInputFields, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return AgentInputs{}, &PyError{Class: "ValueError", Message: "unknown agent inputs: " + strings.Join(unknown, ", ")}
	}
	var inputs AgentInputs
	text := func(key string, path bool) (string, error) {
		v, _ := data.Get(key)
		if v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			if path {
				return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not %s", pyrepr.StrRepr(pyrepr.TypeName(v)))}
			}
			return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got %s", pyrepr.StrRepr(pyrepr.TypeName(v)))}
		}
		if path {
			return pypath.PathStr(s), nil
		}
		return s, nil
	}
	mapping := func(key string, path bool) (map[string]string, error) {
		v, present := data.Get(key)
		if !present {
			return map[string]string{}, nil
		}
		obj, ok := v.(*pyjson.Object)
		if !ok {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'items'", pyrepr.StrRepr(pyrepr.TypeName(v)))}
		}
		out := map[string]string{}
		for _, name := range obj.Keys {
			s, ok := obj.Vals[name].(string)
			if !ok {
				return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not %s", pyrepr.StrRepr(pyrepr.TypeName(obj.Vals[name])))}
			}
			if path {
				s = pypath.PathStr(s)
			}
			out[name] = s
		}
		return out, nil
	}
	var missing []string
	for _, key := range agentInputFields[:9] {
		if _, ok := data.Get(key); !ok {
			missing = append(missing, "'"+key+"'")
		}
	}
	if len(missing) > 0 {
		plural, list := "", missing[0]
		if len(missing) > 1 {
			plural = "s"
			list = strings.Join(missing[:len(missing)-1], ", ") + " and " + missing[len(missing)-1]
			if len(missing) > 2 {
				list = strings.Join(missing[:len(missing)-1], ", ") + ", and " + missing[len(missing)-1]
			}
		}
		return AgentInputs{}, &PyError{Class: "TypeError", Message: fmt.Sprintf("AgentInputs.__init__() missing %d required positional argument%s: %s", len(missing), plural, list)}
	}
	fields := []struct {
		key  string
		path bool
		dest *string
	}{
		{"project", true, &inputs.Project}, {"code", true, &inputs.Code}, {"repo", false, &inputs.Repo},
		{"placement", false, &inputs.Placement}, {"target", false, &inputs.Target}, {"python", true, &inputs.Python},
		{"site_packages", true, &inputs.SitePackages}, {"environment", true, &inputs.Environment},
		{"pm_runtime", true, &inputs.PMRuntime}, {"tools", true, &inputs.Tools}, {"command_dir", true, &inputs.CommandDir},
		{"stamp", true, &inputs.Stamp}, {"features", true, &inputs.Features},
	}
	for _, field := range fields {
		value, err := text(field.key, field.path)
		if err != nil {
			return AgentInputs{}, err
		}
		*field.dest = value
	}
	for _, key := range []string{"bin_dir", "ref"} {
		if v, ok := data.Get(key); ok && v != nil {
			value, err := text(key, false)
			if err != nil {
				return AgentInputs{}, err
			}
			if key == "bin_dir" {
				inputs.BinDir = &value
			} else {
				inputs.Ref = &value
			}
		}
	}
	var err error
	if inputs.Resources, err = mapping("resources", true); err != nil {
		return AgentInputs{}, err
	}
	if inputs.Frontends, err = mapping("frontends", true); err != nil {
		return AgentInputs{}, err
	}
	if v, ok := data.Get("env"); ok && v != nil {
		env, ok := v.(*pyjson.Object)
		if !ok {
			return AgentInputs{}, &PyError{Class: "TypeError", Message: fmt.Sprintf("%s object is not a mapping", pyrepr.StrRepr(pyrepr.TypeName(v)))}
		}
		inputs.Env = env
	}
	return inputs, nil
}

// Refuses inputs whose target, placement, layout or prepared paths an assembly cannot use
func (inputs AgentInputs) validate(out string) error {
	valueError := func(msg string) error { return &PyError{Class: "ValueError", Message: msg} }
	if !targetPattern.MatchString(inputs.Target) {
		return valueError("unsupported target: " + inputs.Target)
	}
	if !slices.Contains([]string{"contained", "fixed", "references"}, inputs.Placement) {
		return valueError("unknown placement: " + inputs.Placement)
	}
	if _, parts := pypath.PathParts(inputs.Repo); filepath.IsAbs(inputs.Repo) || slices.Contains(parts, "..") {
		return valueError("repo must be an output-relative path")
	}
	if !binDirPattern.MatchString(inputs.binDir()) {
		return valueError("bin_dir must be one output-relative directory")
	}
	for name := range inputs.Resources {
		if resourceEnvVar(name) == "" {
			return valueError("unknown resource names")
		}
	}
	for name := range inputs.Frontends {
		if !slices.Contains(frontendNames, name) {
			return valueError("unknown frontend names")
		}
	}
	paths := []string{inputs.Project, inputs.Code, inputs.Python, inputs.SitePackages, inputs.Environment, inputs.PMRuntime}
	for _, name := range orderedKeys(inputs.Resources, resourceEnvNames()) {
		paths = append(paths, inputs.Resources[name])
	}
	for _, name := range orderedKeys(inputs.Frontends, frontendNames) {
		paths = append(paths, inputs.Frontends[name])
	}
	for _, path := range []string{inputs.Tools, inputs.CommandDir, inputs.Stamp, inputs.Features} {
		if path != "" {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return valueError("input path must be absolute: " + pypath.PathStr(path))
		}
		if !pathExists(path) {
			return &PyError{Class: "FileNotFoundError", Message: "required agent input missing: " + path}
		}
	}
	for _, path := range []string{inputs.Project, inputs.Python} {
		if !isRegularFile(path) {
			return valueError("expected a file: " + path)
		}
	}
	for _, path := range []string{inputs.Code, inputs.SitePackages, inputs.Environment, inputs.PMRuntime} {
		if !isDirectory(path) {
			return valueError("expected a directory: " + path)
		}
	}
	marker, err := readJSONObject(filepath.Join(inputs.PMRuntime, "pm-runtime.json"))
	if err != nil {
		return err
	}
	var located [2]string
	for i, key := range []string{"python", "sitePackages"} {
		value, ok := marker.Get(key)
		if !ok {
			return &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
		}
		rel, ok := value.(string)
		if !ok {
			return &PyError{Class: "TypeError", Message: fmt.Sprintf("unsupported operand type(s) for /: 'PosixPath' and %s", pyrepr.StrRepr(pyrepr.TypeName(value)))}
		}
		located[i] = resolvePath(filepath.Join(inputs.PMRuntime, rel))
		if filepath.IsAbs(rel) {
			located[i] = resolvePath(rel)
		}
	}
	if !isRegularFile(located[0]) || !isDirectory(located[1]) {
		return valueError("prepared PM runtime is incomplete")
	}
	if inputs.Stamp != "" {
		stamp, err := readJSONObject(inputs.Stamp)
		if err != nil {
			return err
		}
		distribution, _ := stamp.Get("distribution")
		pmRuntime, _ := stamp.Get("pmRuntime")
		if (distribution == "nix" || distribution == "docker") && pmRuntime != pypath.PathStr(inputs.PMRuntime) {
			return valueError("install stamp must reference the supplied independent PM runtime")
		}
	}
	if inputs.Placement == "contained" {
		for _, path := range []string{inputs.Python, inputs.SitePackages, inputs.Environment, inputs.PMRuntime, inputs.Tools} {
			if path != "" && !isRelativeTo(resolvePath(path), out) {
				return valueError("contained input must be prepared inside output: " + path)
			}
		}
	}
	if inputs.Placement == "references" && inputs.CommandDir == "" {
		return valueError("referenced placement requires command_dir")
	}
	return validateFrontends(inputs.Frontends)
}

// Refuses a frontend product that lacks the files its runtime loads
func validateFrontends(frontends map[string]string) error {
	for _, name := range orderedKeys(frontends, frontendNames) {
		required := []string{"index.html"}
		if name == "tui" {
			required = []string{"dist/entry.js", "package.json"}
		}
		for _, entry := range required {
			path := filepath.Join(frontends[name], filepath.FromSlash(entry))
			if !isRegularFile(path) {
				return &PyError{Class: "FileNotFoundError", Message: path}
			}
		}
	}
	return nil
}

// JSON object of a utf-8-sig file, AttributeError when the document is no object
func readJSONObject(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, &PyError{Class: "JSONDecodeError", Message: err.Error()}
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'get'", pyrepr.StrRepr(pyrepr.TypeName(value)))}
	}
	return obj, nil
}
