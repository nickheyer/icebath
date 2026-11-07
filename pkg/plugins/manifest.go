package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

// Highest manifest schema version this build understands
const SupportedManifestVersion = 2

// Plugin kinds a manifest may declare
var validPluginKinds = map[string]bool{"standalone": true, "backend": true, "exclusive": true, "platform": true, "model-provider": true}

// Manifest fields the loader knows, every other field warns and is ignored
var knownManifestFields = map[string]bool{
	"name": true, "version": true, "description": true, "author": true, "requires_env": true, "provides_tools": true,
	"provides_hooks": true, "kind": true, "hooks": true, "label": true, "optional_env": true, "platforms": true,
	"external_dependencies": true, "pip_dependencies": true, "provides_browser_providers": true,
	"provides_web_providers": true, "manifest_version": true, "api_version": true, "requires_plugins": true,
	"python_dependencies": true, "config_schema": true, "license": true, "homepage": true, "tags": true,
	"capabilities": true, "emits": true, "listens": true, "hermes": true, "icebath": true, "depends": true,
	"requires_hermes": true, "requires_icebath": true, "python_runtime": true, "provides_locales": true,
	"requires_auth": true, "oauth": true, "config_keys": true, "update_url": true,
	"entry": true, "skills": true, "commands": true, "mcp_servers": true, "settings": true, "provides_middleware": true,
}

// Config schema type names and the Go kinds a setting value must have
var ConfigSchemaTypes = map[string][]string{
	"str": {"string"}, "string": {"string"}, "int": {"int"}, "integer": {"int"}, "float": {"int", "float"},
	"number": {"int", "float"}, "bool": {"bool"}, "boolean": {"bool"}, "list": {"list"}, "array": {"list"},
	"dict": {"map"}, "object": {"map"}, "secret": {"string"},
}

var languageIDRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})*$`)

// Parsed plugin.yaml or portable plugin.json manifest
type Manifest struct {
	Name              string
	Version           string
	Description       string
	Author            string
	RequiresEnv       []EnvSpec
	ProvidesTools     []string
	ProvidesHooks     []string
	ProvidesMiddlewar []string
	Source            string
	Path              string
	Kind              string
	Key               string
	RequiresIcebath   string
	Portable          bool
	DataNamespace     string
	Capabilities      []string
	ManifestVersion   int
	APIVersion        *int
	RequiresPlugins   []PluginDependency
	PythonDeps        []string
	PythonRuntime     string
	ConfigSchema      map[string]map[string]any
	License           string
	Homepage          string
	Tags              []string
	Emits             []string
	Listens           []string
	ProvidesLocales   []string
	LocaleMetadata    map[string]map[string]any
	ConfigKeys        []string
	UpdateURL         string
	RequiresAuth      []string
	OAuth             []map[string]any
	Entry             []string
	SkillDirs         []string
	Commands          []CommandSpec
	MCPServers        map[string]map[string]any
	Raw               map[string]any
}

// One requires_env entry with the install prompt metadata
type EnvSpec struct {
	Name        string
	Description string
	URL         string
	Secret      bool
}

// One advisory requires_plugins entry
type PluginDependency struct {
	ID           string
	VersionRange string
}

// One slash command a manifest declares for its executable entry
type CommandSpec struct {
	Name         string
	Description  string
	ArgsHint     string
	ArgumentMode string
}

// Registry id of a manifest, the path derived key when set, else the bare name
func (m *Manifest) ID() string {
	if m.Key != "" {
		return m.Key
	}
	return m.Name
}

// Reports whether the manifest names an executable that speaks the plugin host protocol
func (m *Manifest) HasEntry() bool { return len(m.Entry) > 0 }

// Reports whether the plugin loads from declared files alone with no code to run
func (m *Manifest) ManifestOnly() bool {
	if m.HasEntry() {
		return false
	}
	_, err := os.Stat(filepath.Join(m.Path, "__init__.py"))
	return err != nil
}

// Canonical lowercase hyphenated language id
func normalizeLanguageID(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

// Reports whether v is a language id such as pl or pt-br
func isLanguageID(v any) bool { return languageIDRe.MatchString(normalizeLanguageID(v)) }

// Collision resistant plugin-data directory name for a portable plugin
func portableDataNamespace(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "agent-plugin-" + portableSlug(key) + "-" + hex.EncodeToString(sum[:])[:8]
}

// Lowercase slug keeping ASCII letters digits underscore and dash
func portableSlug(key string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(key) {
		if ch < 128 && (ch == '_' || ch == '-' || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
			b.WriteRune(ch)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-_")
	if slug == "" {
		return "plugin"
	}
	return slug
}

// Internal name of a portable plugin MCP server, the slug of its mcp.json name
func portableMCPServerName(server string) string { return portableSlug(server) }

// Author text from a string or a name email url mapping
func displayAuthor(v any) string {
	switch a := v.(type) {
	case nil:
		return ""
	case map[string]any:
		var parts []string
		for _, f := range []string{"name", "email", "url"} {
			if s, ok := a[f].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// Strings of a list value, every non string item dropped
func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch s := it.(type) {
		case string:
			out = append(out, s)
		case int, int64, float64:
			out = append(out, fmt.Sprint(s))
		}
	}
	return out
}

// Every value of a map as a string keyed map, nil when v is no mapping
func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out
	}
	return nil
}

// Integer of a YAML scalar
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// Parsed requires_env entries, nameless entries dropped
func parseEnvSpecs(v any) []EnvSpec {
	var out []EnvSpec
	for _, it := range toList(v) {
		switch e := it.(type) {
		case string:
			out = append(out, EnvSpec{Name: e})
		default:
			if m := asMap(e); m != nil {
				name, _ := m["name"].(string)
				if name == "" {
					continue
				}
				desc, _ := m["description"].(string)
				url, _ := m["url"].(string)
				sec, _ := m["secret"].(bool)
				out = append(out, EnvSpec{Name: name, Description: desc, URL: url, Secret: sec})
			}
		}
	}
	return out
}

// Items of a list value or nil
func toList(v any) []any {
	items, _ := v.([]any)
	return items
}

// Dotted config path at least two segments deep, empty when malformed
func configKeyPath(item any) string {
	s, ok := item.(string)
	if !ok {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 2 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
	}
	return strings.Join(parts, ".")
}

const unknownFieldsWarning = "Plugin %s: unknown manifest field(s) ignored: %s (newer manifest schema or typo; plugin still loads)"

// Python int of a manifest scalar, warning with the formatted repr and reporting false when it does not convert
func manifestInt(raw any, key, warn string) (int, bool) {
	switch v := raw.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return int(v), true
		}
	case string:
		digits := strings.TrimSpace(v)
		body := strings.TrimLeft(digits, "+-")
		if len(digits)-len(body) <= 1 && body != "" && !strings.HasPrefix(body, "_") && !strings.HasSuffix(body, "_") && !strings.Contains(body, "__") {
			if n, err := strconv.ParseInt(strings.ReplaceAll(digits, "_", ""), 10, 64); err == nil {
				return int(n), true
			}
		}
	}
	logWarn(fmt.Sprintf(warn, key, config.PyRepr(raw)))
	return 0, false
}

// List valued manifest field, warning and yielding nothing when present with another type
func manifestFieldList(data map[string]any, key, field, what string) []any {
	raw := data[field]
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		logWarn(fmt.Sprintf("Plugin %s: %s must be %s; ignoring", key, field, what))
		return nil
	}
	return items
}

// Validated config_keys of a manifest mapping
func manifestConfigKeys(data map[string]any, key string) []string {
	var out []string
	for _, item := range manifestFieldList(data, key, "config_keys", "a list of dotted config paths") {
		p := configKeyPath(item)
		if p == "" {
			logWarn(fmt.Sprintf("Plugin %s: config_keys entry %s must be a dotted path at least two levels deep; skipping", key, config.PyRepr(item)))
			continue
		}
		out = append(out, p)
	}
	return out
}

// Config schema entries keeping mapping specs and warning on unknown types
func configSchemaEntries(key string, raw any) map[string]map[string]any {
	schema := map[string]map[string]any{}
	for skey, spec := range asMap(raw) {
		sm := asMap(spec)
		if sm == nil {
			logWarn(fmt.Sprintf("Plugin %s: config_schema entry %s must be a mapping (e.g. {type: str}); skipping", key, config.PyRepr(skey)))
			continue
		}
		if t, ok := sm["type"]; ok && t != nil {
			if _, known := ConfigSchemaTypes[strings.ToLower(fmt.Sprint(t))]; !known {
				logWarn(fmt.Sprintf("Plugin %s: config_schema key %s declares unknown type %s (known: %s); type check will be skipped for it",
					key, config.PyRepr(skey), config.PyRepr(t), strings.Join(sortedKeys(ConfigSchemaTypes), ", ")))
			}
		}
		schema[skey] = sm
	}
	return schema
}

// Provides locales ids and per id metadata with invalid or duplicate ids dropped
func parseProvidesLocales(raw any, key string) ([]string, map[string]map[string]any) {
	var ids []string
	meta := map[string]map[string]any{}
	if raw == nil {
		return ids, meta
	}
	var items []any
	switch r := raw.(type) {
	case string:
		items = []any{r}
	case []any:
		items = r
	default:
		if m := asMap(raw); m != nil {
			items = []any{m}
		} else {
			logWarn(fmt.Sprintf("Plugin %s: provides_locales must be a list of language ids, got %s", key, config.PyTypeName(raw)))
			return ids, meta
		}
	}
	for _, item := range items {
		var lang string
		entry := map[string]any{}
		if m := asMap(item); m != nil {
			lang = normalizeLanguageID(m["id"])
			if e, ok := m["endonym"].(string); ok && strings.TrimSpace(e) != "" {
				entry["endonym"] = strings.TrimSpace(e)
			}
			if r, ok := m["rtl"]; ok {
				entry["rtl"] = config.PyTruthy(r)
			}
		} else {
			lang = normalizeLanguageID(item)
		}
		if !isLanguageID(lang) {
			logWarn(fmt.Sprintf("Plugin %s: ignoring invalid provides_locales entry %s", key, config.PyRepr(item)))
			continue
		}
		dup := false
		for _, have := range ids {
			if have == lang {
				dup = true
			}
		}
		if dup {
			logWarn(fmt.Sprintf("Plugin %s: duplicate provides_locales entry %s", key, config.PyRepr(lang)))
			continue
		}
		ids = append(ids, lang)
		if len(entry) > 0 {
			meta[lang] = entry
		}
	}
	return ids, meta
}

// Kind implied by provider markers in an init file, empty when none match
func detectKindFromSource(src string) string {
	for _, marker := range []string{"register_memory_provider", "MemoryProvider", "register_cron_scheduler", "CronScheduler", "register_computer_use_provider", "ComputerUseProvider"} {
		if strings.Contains(src, marker) {
			return "exclusive"
		}
	}
	if strings.Contains(src, "register_provider") && strings.Contains(src, "ProviderProfile") {
		return "model-provider"
	}
	return ""
}

// Normalized kind with undeclared provider plugins detected from their init file
func manifestKind(data map[string]any, key, dir string) string {
	kind := "standalone"
	if raw, ok := data["kind"].(string); ok {
		kind = strings.ToLower(strings.TrimSpace(raw))
	}
	if !validPluginKinds[kind] {
		logWarn(fmt.Sprintf("Plugin %s: unknown kind '%s' (valid: %s); treating as 'standalone'", key, config.PyStr(data["kind"]), strings.Join(sortedKeys(validPluginKinds), ", ")))
		kind = "standalone"
	}
	if _, declared := data["kind"]; kind == "standalone" && !declared {
		if b, err := os.ReadFile(filepath.Join(dir, "__init__.py")); err == nil {
			if d := detectKindFromSource(string(b[:min(len(b), 8192)])); d != "" {
				kind = d
			}
		}
	}
	return kind
}

// Entry argv of a manifest from a string or list value
func parseEntry(v any) []string {
	switch e := v.(type) {
	case string:
		if strings.TrimSpace(e) == "" {
			return nil
		}
		return splitCommandLine(e)
	case []any:
		return stringList(e)
	}
	return nil
}

// Declared slash commands of a manifest
func parseCommands(v any, key string) []CommandSpec {
	var out []CommandSpec
	for _, item := range toList(v) {
		switch c := item.(type) {
		case string:
			out = append(out, CommandSpec{Name: c})
		default:
			m := asMap(item)
			if m == nil {
				slog.Warn("plugin commands entry must be a name or mapping", "plugin", key, "entry", item)
				continue
			}
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := m["description"].(string)
			hint, _ := m["args_hint"].(string)
			mode, _ := m["argument_mode"].(string)
			out = append(out, CommandSpec{Name: name, Description: desc, ArgsHint: hint, ArgumentMode: mode})
		}
	}
	return out
}

// Declared MCP servers of a manifest keyed by server name
func parseMCPServers(v any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, cfg := range asMap(v) {
		if m := asMap(cfg); m != nil {
			out[name] = m
		}
	}
	return out
}

// Skill directories a manifest declares, the conventional skills folder when present and undeclared
func parseSkillDirs(v any, dir string) []string {
	var out []string
	if v == nil {
		if st, err := os.Stat(filepath.Join(dir, "skills")); err == nil && st.IsDir() {
			return []string{filepath.Join(dir, "skills")}
		}
		return nil
	}
	for _, s := range stringList(v) {
		if filepath.IsAbs(s) {
			continue
		}
		p := filepath.Clean(filepath.Join(dir, s))
		if rel, err := filepath.Rel(dir, p); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Parses one plugin.yaml file into a manifest, nil with a warning on failure
func ParseManifestFile(file, dir, source, prefix string) *Manifest {
	raw, err := os.ReadFile(file)
	if err != nil {
		logWarn(fmt.Sprintf("Failed to parse %s: %v", file, err), excInfo(err)...)
		return nil
	}
	var doc any
	if err := yaml.Unmarshal(stripBOM(raw), &doc); err != nil {
		logWarn(fmt.Sprintf("Failed to parse %s: %v", file, err), excInfo(err)...)
		return nil
	}
	if !config.PyTruthy(doc) {
		doc = map[string]any{}
	}
	data := asMap(doc)
	if data == nil {
		logWarn(fmt.Sprintf("Failed to parse %s: top level must be a mapping, got %s (#14066)", file, config.PyTypeName(doc)))
		return nil
	}
	if data == nil {
		data = map[string]any{}
	}
	return manifestFromData(data, dir, source, prefix)
}

// Manifest built from a decoded plugin.yaml mapping
func manifestFromData(data map[string]any, dir, source, prefix string) *Manifest {
	data = normalizeAnyMaps(data)
	name, _ := data["name"].(string)
	if name == "" {
		name = filepath.Base(dir)
	}
	key := name
	if prefix != "" {
		key = prefix + "/" + filepath.Base(dir)
	}
	m := &Manifest{Name: name, Source: source, Path: dir, Key: key, Raw: data, ManifestVersion: 1}
	m.Version = scalarString(data["version"])
	m.Description, _ = data["description"].(string)
	m.Author = displayAuthor(data["author"])
	m.RequiresEnv = parseEnvSpecs(data["requires_env"])
	m.ProvidesTools = stringList(data["provides_tools"])
	if hooks, ok := data["provides_hooks"]; ok {
		m.ProvidesHooks = stringList(hooks)
	} else {
		m.ProvidesHooks = stringList(data["hooks"])
	}
	m.ProvidesMiddlewar = stringList(data["provides_middleware"])
	m.Kind = manifestKind(data, key, dir)
	m.RequiresIcebath = manifestRequires(data)
	m.Capabilities = ParseDeclaredCapabilities(data["capabilities"], name)
	mvRaw, ok := data["manifest_version"]
	if !ok {
		mvRaw = 1
	}
	if n, ok := manifestInt(mvRaw, key, "Plugin %s: manifest_version %s is not an integer; treating as 1"); ok {
		m.ManifestVersion = n
	}
	if m.ManifestVersion > SupportedManifestVersion {
		logWarn(fmt.Sprintf("Plugin %s: manifest_version %d is newer than this icebath supports (%d); loading anyway and ignoring unknown fields",
			key, m.ManifestVersion, SupportedManifestVersion))
	}
	if av := data["api_version"]; av != nil {
		if n, ok := manifestInt(av, key, "Plugin %s: api_version %s is not an integer; ignoring"); ok {
			m.APIVersion = &n
		}
	}
	for _, item := range manifestFieldList(data, key, "requires_plugins", "a list") {
		switch d := item.(type) {
		case string:
			m.RequiresPlugins = append(m.RequiresPlugins, PluginDependency{ID: d})
		default:
			dm := asMap(item)
			id, _ := dm["id"].(string)
			if dm == nil || id == "" {
				logWarn(fmt.Sprintf("Plugin %s: requires_plugins entry %s must be a plugin id string or a {id, version_range} mapping; skipping", key, config.PyRepr(item)))
				continue
			}
			vr := ""
			if dm["version_range"] != nil {
				vr = config.PyStr(dm["version_range"])
			}
			m.RequiresPlugins = append(m.RequiresPlugins, PluginDependency{ID: id, VersionRange: vr})
		}
	}
	for _, item := range manifestFieldList(data, key, "python_dependencies", "a list of requirement strings") {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			logWarn(fmt.Sprintf("Plugin %s: python_dependencies entry %s must be a non-empty requirement string; skipping", key, config.PyRepr(item)))
			continue
		}
		m.PythonDeps = append(m.PythonDeps, strings.TrimSpace(s))
	}
	m.PythonRuntime = strings.ToLower(strings.TrimSpace(scalarString(data["python_runtime"])))
	schemaRaw := data["config_schema"]
	if schemaRaw != nil && asMap(schemaRaw) == nil {
		logWarn(fmt.Sprintf("Plugin %s: config_schema must be a mapping; ignoring", key))
		schemaRaw = nil
	}
	m.ConfigSchema = configSchemaEntries(key, schemaRaw)
	m.License, m.Homepage = "", ""
	if config.PyTruthy(data["license"]) {
		m.License = config.PyStr(data["license"])
	}
	if config.PyTruthy(data["homepage"]) {
		m.Homepage = config.PyStr(data["homepage"])
	}
	m.Tags = []string{}
	for _, t := range manifestFieldList(data, key, "tags", "a list") {
		m.Tags = append(m.Tags, config.PyStr(t))
	}
	m.Emits = stringList(data["emits"])
	m.Listens = stringList(data["listens"])
	m.ProvidesLocales, m.LocaleMetadata = parseProvidesLocales(data["provides_locales"], key)
	m.ConfigKeys = manifestConfigKeys(data, key)
	m.UpdateURL = strings.TrimSpace(scalarString(data["update_url"]))
	for _, p := range stringList(data["requires_auth"]) {
		if strings.TrimSpace(p) != "" {
			m.RequiresAuth = append(m.RequiresAuth, strings.TrimSpace(p))
		}
	}
	for _, item := range toList(data["oauth"]) {
		if om := asMap(item); om != nil {
			m.OAuth = append(m.OAuth, om)
		}
	}
	m.Entry = parseEntry(data["entry"])
	m.SkillDirs = parseSkillDirs(data["skills"], dir)
	m.Commands = parseCommands(data["commands"], key)
	m.MCPServers = parseMCPServers(data["mcp_servers"])
	var unknown []string
	for k := range data {
		if !knownManifestFields[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		if m.ManifestVersion >= 2 {
			logWarn(fmt.Sprintf(unknownFieldsWarning, key, strings.Join(unknown, ", ")))
		} else {
			Logger.Debug(fmt.Sprintf(unknownFieldsWarning, key, strings.Join(unknown, ", ")))
		}
	}
	return m
}

// String form of a scalar manifest value, empty for nil
func scalarString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Slice with s appended when absent
func appendUnique(items []string, s string) []string {
	for _, have := range items {
		if have == s {
			return items
		}
	}
	return append(items, s)
}

// Document with every nested mapping converted to a string keyed map
func normalizeAnyMaps(v map[string]any) map[string]any {
	for k, val := range v {
		switch c := val.(type) {
		case map[any]any:
			v[k] = normalizeAnyMaps(asMap(c))
		case map[string]any:
			v[k] = normalizeAnyMaps(c)
		case []any:
			for i, it := range c {
				if m := asMap(it); m != nil {
					c[i] = normalizeAnyMaps(m)
				}
			}
		}
	}
	return v
}

// Bytes without a UTF-8 byte order mark
func stripBOM(b []byte) []byte {
	return []byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
}

// Warning strings for settings that violate the config schema
func ValidateConfigSchema(pluginID string, schema map[string]map[string]any, settings map[string]any) []string {
	var warnings []string
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, skey := range keys {
		spec := schema[skey]
		value, present := settings[skey]
		if !present {
			if req, _ := spec["required"].(bool); req {
				if _, hasDefault := spec["default"]; !hasDefault {
					warnings = append(warnings, fmt.Sprintf("plugins.entries.%s.settings.%s is required by the plugin's config_schema but is not set", pluginID, skey))
				}
			}
			continue
		}
		stype, _ := spec["type"].(string)
		if stype == "" {
			continue
		}
		expected, ok := ConfigSchemaTypes[strings.ToLower(stype)]
		if !ok {
			continue
		}
		if !valueHasKind(value, expected) {
			warnings = append(warnings, fmt.Sprintf("plugins.entries.%s.settings.%s should be %s (got %s)", pluginID, skey, stype, kindName(value)))
		}
	}
	return warnings
}

// Kind name of a decoded YAML value in config schema vocabulary
func kindName(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case int, int64:
		return "int"
	case float64, float32:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any, map[any]any:
		return "dict"
	case nil:
		return "NoneType"
	}
	return fmt.Sprintf("%T", v)
}

// Reports whether v is one of the expected schema kinds
func valueHasKind(v any, expected []string) bool {
	k := kindName(v)
	for _, e := range expected {
		switch e {
		case "string":
			if k == "str" {
				return true
			}
		case "int":
			if k == "int" {
				return true
			}
		case "float":
			if k == "float" {
				return true
			}
		case "bool":
			if k == "bool" {
				return true
			}
		case "list":
			if k == "list" {
				return true
			}
		case "map":
			if k == "dict" {
				return true
			}
		}
	}
	return false
}

// Plugin keys in dependency order with alphabetical ties, alphabetical for all on a cycle
func ResolveLoadOrder(manifests map[string]*Manifest) []string {
	keys := make([]string, 0, len(manifests))
	for k := range manifests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	byName := map[string]string{}
	for _, k := range keys {
		if n := manifests[k].Name; n != "" {
			if _, have := byName[n]; !have {
				byName[n] = k
			}
		}
	}
	edges := map[string]map[string]bool{}
	for _, k := range keys {
		edges[k] = map[string]bool{}
		for _, dep := range manifests[k].RequiresPlugins {
			if dep.ID == "" {
				continue
			}
			resolved := ""
			if _, ok := manifests[dep.ID]; ok {
				resolved = dep.ID
			} else if r, ok := byName[dep.ID]; ok {
				resolved = r
			}
			switch {
			case resolved == "":
				logWarn(fmt.Sprintf("Plugin %s requires plugin '%s' which is not enabled/installed; loading anyway (probe availability at runtime via ctx.has_plugin). Run `icebath plugins enable %s` if it is installed.", k, dep.ID, dep.ID))
			case resolved == k:
				logWarn(fmt.Sprintf("Plugin %s declares a dependency on itself; ignoring", k))
			default:
				edges[k][resolved] = true
			}
		}
	}
	if cycle := loadOrderCycle(keys, edges); cycle != nil {
		logWarn(fmt.Sprintf("Plugin dependency cycle detected (%s); falling back to alphabetical load order for all plugins", strings.Join(cycle, " -> ")))
		return keys
	}
	var ordered []string
	done := map[string]bool{}
	for len(ordered) < len(keys) {
		var ready []string
		for _, k := range keys {
			if done[k] {
				continue
			}
			ok := true
			for dep := range edges[k] {
				if !done[dep] {
					ok = false
				}
			}
			if ok {
				ready = append(ready, k)
			}
		}
		for _, k := range ready {
			done[k] = true
		}
		ordered = append(ordered, ready...)
	}
	return ordered
}

// First cycle in graphlib TopologicalSorter order, dependencies leading to dependents, closing on its first node
func loadOrderCycle(keys []string, edges map[string]map[string]bool) []string {
	var nodes []string
	known := map[string]bool{}
	successors := map[string][]string{}
	add := func(n string) {
		if !known[n] {
			known[n] = true
			nodes = append(nodes, n)
		}
	}
	for _, k := range keys {
		add(k)
		for _, dep := range sortedKeys(edges[k]) {
			add(dep)
			successors[dep] = append(successors[dep], k)
		}
	}
	seen := map[string]bool{}
	stackIndex := map[string]int{}
	var stack []string
	var next []int
	for _, start := range nodes {
		if seen[start] {
			continue
		}
		n := start
		for {
			if !seen[n] {
				seen[n] = true
				stackIndex[n] = len(stack)
				stack = append(stack, n)
				next = append(next, 0)
			} else if i, onStack := stackIndex[n]; onStack {
				return append(append([]string{}, stack[i:]...), n)
			}
			advanced := false
			for len(stack) > 0 {
				top := len(stack) - 1
				if i := next[top]; i < len(successors[stack[top]]) {
					next[top]++
					n = successors[stack[top]][i]
					advanced = true
					break
				}
				delete(stackIndex, stack[top])
				stack, next = stack[:top], next[:top]
			}
			if !advanced {
				break
			}
		}
	}
	return nil
}

// Splits a command line into argv the way a POSIX shell lexes quotes and backslashes
func splitCommandLine(line string) []string {
	var out []string
	var cur strings.Builder
	inTok, sq, dq, esc := false, false, false, false
	for _, ch := range line {
		switch {
		case esc:
			cur.WriteRune(ch)
			esc = false
		case sq:
			if ch == '\'' {
				sq = false
			} else {
				cur.WriteRune(ch)
			}
		case dq:
			switch ch {
			case '"':
				dq = false
			case '\\':
				esc = true
			default:
				cur.WriteRune(ch)
			}
		case ch == '\\':
			esc, inTok = true, true
		case ch == '\'':
			sq, inTok = true, true
		case ch == '"':
			dq, inTok = true, true
		case ch == ' ' || ch == '\t' || ch == '\n':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(ch)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}
