package plugins

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

var upperSnakeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*\n?$`)

const (
	probeTimeout  = 30
	probeSentinel = "ICEBATH_VALIDATE_JSON:"
)

// One admission check outcome
type ValidationCheck struct {
	Name   string
	OK     bool
	Detail string
}

// Result of validating one plugin directory, isolation informational only
type ValidationReport struct {
	Checks    []ValidationCheck
	Warnings  []string
	Isolation *IsolationReport
}

// Detail of every failed check, its name when the detail is empty
func (r *ValidationReport) Failures() []string {
	out := []string{}
	for _, c := range r.Checks {
		if !c.OK {
			if c.Detail != "" {
				out = append(out, c.Detail)
			} else {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

// Reports whether every check passed
func (r *ValidationReport) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Zero when every check passed, one otherwise
func (r *ValidationReport) ExitCode() int {
	if r.OK() {
		return 0
	}
	return 1
}

func (r *ValidationReport) add(name string, ok bool, detail string) {
	r.Checks = append(r.Checks, ValidationCheck{Name: name, OK: ok, Detail: detail})
}

func (r *ValidationReport) warn(message string) { r.Warnings = append(r.Warnings, message) }

// JSON document of the report in the upstream key order
func (r *ValidationReport) ToDict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("ok", r.OK())
	checks := make([]any, 0, len(r.Checks))
	for _, c := range r.Checks {
		item := pyjson.NewObject()
		item.Set("name", c.Name)
		item.Set("ok", c.OK)
		item.Set("detail", c.Detail)
		checks = append(checks, item)
	}
	out.Set("checks", checks)
	warnings := make([]any, len(r.Warnings))
	for i, w := range r.Warnings {
		warnings[i] = w
	}
	out.Set("warnings", warnings)
	if r.Isolation != nil {
		out.Set("isolation", r.Isolation.ToDict())
	} else {
		out.Set("isolation", nil)
	}
	return out
}

// Strict requires_icebath parse that rejects any clause whose version does not parse
func requiresIcebathSpecValid(spec string) bool {
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		target := clause
		if m := versionComparatorRe.FindStringSubmatch(clause); m != nil {
			target = m[2]
		}
		if versionTuple(target) == nil {
			return false
		}
	}
	return true
}

func checkManifestFields(report *ValidationReport, manifest map[string]any) {
	missing := []string{}
	for _, f := range []string{"name", "version", "description"} {
		if !config.PyTruthy(manifest[f]) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		report.add("manifest fields", false, "plugin.yaml missing required field(s): "+strings.Join(missing, ", "))
	} else {
		report.add("manifest fields", true, "name, version, description present")
	}
}

// The requires_icebath value, requires_hermes when the icebath key is empty, as the manifest loader reads it
func manifestRequiresSpec(manifest map[string]any) string {
	for _, key := range []string{"requires_icebath", "requires_hermes"} {
		if v := manifest[key]; config.PyTruthy(v) {
			if spec := strings.TrimSpace(config.PyStr(v)); spec != "" {
				return spec
			}
		}
	}
	return ""
}

func checkRequiresIcebath(report *ValidationReport, manifest map[string]any) {
	spec := manifestRequiresSpec(manifest)
	switch {
	case spec == "":
		report.add("requires_icebath", true, "not declared")
	case requiresIcebathSpecValid(spec):
		report.add("requires_icebath", true, fmt.Sprintf("spec %s parses", pyrepr.Repr(spec)))
	default:
		report.add("requires_icebath", false, fmt.Sprintf("requires_icebath spec %s does not parse (expected e.g. \">=0.19\" or \">=0.19, <1.0\")", pyrepr.Repr(spec)))
	}
}

// Keys of a mapping in the given source order, keys missing from the order appended sorted
func orderedMapKeys(m map[string]any, order []string) []string {
	out := make([]string, 0, len(m))
	seen := map[string]bool{}
	for _, k := range order {
		if _, ok := m[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, k := range sortedKeys(m) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// Validates the config_schema mapping shape, keys visited in their manifest order
func checkConfigSpec(report *ValidationReport, raw any, order []string) {
	if raw == nil {
		report.add("config schema", true, "not declared")
		return
	}
	switch v := raw.(type) {
	case []any:
		if len(v) == 0 {
			report.add("config schema", true, "not declared")
			return
		}
	case map[string]any:
		if len(v) == 0 {
			report.add("config schema", true, "not declared")
			return
		}
	}
	problems := []string{}
	schema, ok := raw.(map[string]any)
	if !ok {
		problems = append(problems, "config_schema: must be a mapping of key -> spec")
	} else {
		types := sortedKeys(ConfigSchemaTypes)
		for _, skey := range orderedMapKeys(schema, order) {
			spec, ok := schema[skey].(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("config_schema.%s: must be a mapping (e.g. {type: str})", skey))
				continue
			}
			if typ, present := spec["type"]; present && typ != nil {
				if _, known := ConfigSchemaTypes[strings.ToLower(config.PyStr(typ))]; !known {
					problems = append(problems, fmt.Sprintf("config_schema.%s: type must be one of %s", skey, strings.Join(types, "/")))
				}
			}
			if required, present := spec["required"]; present && required != nil {
				if _, isBool := required.(bool); !isBool {
					problems = append(problems, fmt.Sprintf("config_schema.%s: required must be a boolean", skey))
				}
			}
		}
	}
	if len(problems) > 0 {
		report.add("config schema", false, strings.Join(problems, "; "))
	} else {
		report.add("config schema", true, "shape valid")
	}
}

func checkRequiresEnv(report *ValidationReport, manifest map[string]any) {
	raw := manifest["requires_env"]
	if !config.PyTruthy(raw) {
		raw = []any{}
	}
	problems := []string{}
	entries, ok := raw.([]any)
	if !ok {
		problems = append(problems, "requires_env: must be a list")
	}
	for i, entry := range entries {
		var name string
		switch e := entry.(type) {
		case string:
			name = e
		case map[string]any:
			if config.PyTruthy(e["name"]) {
				name = config.PyStr(e["name"])
			}
		default:
			problems = append(problems, fmt.Sprintf("requires_env[%d]: must be a string or mapping", i))
			continue
		}
		if !upperSnakeRe.MatchString(name) {
			problems = append(problems, fmt.Sprintf("requires_env[%d]: %s is not UPPER_SNAKE_CASE", i, pyrepr.Repr(name)))
		}
	}
	if len(problems) > 0 {
		report.add("requires_env", false, strings.Join(problems, "; "))
	} else {
		report.add("requires_env", true, "all entries UPPER_SNAKE")
	}
}

// OAuth client admission, shape and https failures fail while reviewer cues only warn
func checkOAuth(report *ValidationReport, manifest map[string]any, pluginDir string) error {
	if v := manifest["oauth"]; v == nil {
		report.add("oauth", true, "not declared")
		return nil
	} else if list, ok := v.([]any); ok && len(list) == 0 {
		report.add("oauth", true, "not declared")
		return nil
	}
	problemsOf, err := needService("OAuthAdmissionProblems", func(s *Services) func(map[string]any, string) []string { return s.OAuthAdmissionProblems })
	if err != nil {
		return err
	}
	warningsOf, err := needService("OAuthAdmissionWarnings", func(s *Services) func(map[string]any, string) []string { return s.OAuthAdmissionWarnings })
	if err != nil {
		return err
	}
	problems := problemsOf(manifest, pluginDir)
	detail := strings.Join(problems, "; ")
	if detail == "" {
		detail = "clients valid"
	}
	report.add("oauth", len(problems) == 0, detail)
	for _, w := range warningsOf(manifest, pluginDir) {
		report.warn(w)
	}
	return nil
}

// Self contained harness importing the plugin and calling register against a recording context
const probeScript = `
import importlib.util
import json
import sys

plugin_dir = sys.argv[1]
sentinel = sys.argv[2]
options = json.loads(sys.argv[3])
context_methods = set(options["context_methods"])
provider_kind = options["kind"] == "model-provider"

recorded = {"tools": [], "hooks": [], "middleware": [], "commands": [], "providers": [], "trusted_inbound": []}


class RecordingContext:
    plugin_config = {}
    profile_name = "default"
    plugin_id = "icebath_validate_probe_plugin"

    def register_tool(self, name, *args, **kwargs):
        recorded["tools"].append(str(name))

    def register_hook(self, hook_name, callback, *, hold_stream=False):
        recorded["hooks"].append(str(hook_name))

    def register_middleware(self, kind, callback):
        recorded["middleware"].append(str(kind))

    def register_command(self, name, *args, **kwargs):
        recorded["commands"].append(str(name))

    def register_cli_command(self, name, *args, **kwargs):
        recorded["commands"].append(str(name))

    def register_platform(self, name, *args, **kwargs):
        if kwargs.get("trusted_inbound"):
            recorded["trusted_inbound"].append(str(name))

    def get_config(self, key, default=None):
        return default

    def __getattr__(self, name):
        if name in context_methods:
            def _noop(*args, **kwargs):
                return None

            return _noop
        raise AttributeError(name)


def emit(payload):
    print(sentinel + json.dumps(payload))


if provider_kind:
    try:
        import providers as _providers
    except Exception as exc:
        emit({"error": "model-provider probe could not import providers: %s" % exc})
        sys.exit(0)
    _real_register_provider = _providers.register_provider

    def _recording_register_provider(profile):
        recorded["providers"].append(str(getattr(profile, "name", profile)))
        return _real_register_provider(profile)

    _providers.register_provider = _recording_register_provider

try:
    spec = importlib.util.spec_from_file_location(
        "icebath_validate_probe_plugin",
        plugin_dir + "/__init__.py",
        submodule_search_locations=[plugin_dir],
    )
    module = importlib.util.module_from_spec(spec)
    module.__path__ = [plugin_dir]
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
except Exception as exc:
    emit({"error": "import failed: %s" % exc})
    sys.exit(0)

if provider_kind:
    if not recorded["providers"]:
        emit({"error": "model-provider plugin registered no ProviderProfile at import"})
    else:
        emit(recorded)
    sys.exit(0)

register = getattr(module, "register", None)
if register is None:
    emit({"error": "no register() function"})
    sys.exit(0)

try:
    register(RecordingContext())
except Exception as exc:
    emit({"error": "register() raised: %s" % exc})
    sys.exit(0)

emit(recorded)
`

// Public callable attributes of the plugin context class, the scoped provider registrars excluded
var pluginContextStaticMethods = []string{
	"apost_message", "ask_choice", "call_mcp", "current_cron_execution", "dispatch_tool", "emit", "get_config",
	"has_capability", "has_plugin", "inject_message", "on_unload", "post_message", "register_approval_transport",
	"register_automation_blueprint", "register_auxiliary_task", "register_cli_command", "register_command",
	"register_context_engine", "register_context_reference", "register_dashboard_auth_provider", "register_hook",
	"register_locale", "register_locale_dir", "register_memory_provider", "register_middleware", "register_platform",
	"register_platform_handler", "register_redaction_patterns", "register_skill", "register_slack_action_handler",
	"register_system_prompt_section", "register_telegram_handler", "register_tool", "session_cwd", "session_surface",
	"set_config", "side_question", "spawn_task", "subscribe",
}

// Sorted public method names of the plugin context the recording context accepts as no ops
func pluginContextMethods() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, name := range pluginContextStaticMethods {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, row := range scopedProviderRegistrars {
		if !seen[row.method] {
			seen[row.method] = true
			out = append(out, row.method)
		}
	}
	sort.Strings(out)
	return out
}

// String items of a manifest list, empty when the key is not a list
func declaredList(manifest map[string]any, key string) []string {
	raw, ok := manifest[key].([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func checkBuiltinCollisions(report *ValidationReport, manifest map[string]any) {
	checkBuiltinCollisionsWith(report, manifest, nil)
}

// Sorted candidate tool names that a built in tool already holds
func builtinToolCollisions(candidates map[string]bool) []string {
	collisions := []string{}
	for _, name := range tools.Default.Names() {
		if candidates[name] {
			collisions = append(collisions, name)
		}
	}
	sort.Strings(collisions)
	return collisions
}

var loadableEntrypoints = []string{"desktop/plugin.js", "plugin.json"}

// Fails a manifest with nothing beside it that icebath can load
func checkLoadable(report *ValidationReport, pluginDir string, manifest map[string]any) {
	present := []string{}
	for _, rel := range loadableEntrypoints {
		if isFile(filepath.Join(pluginDir, filepath.FromSlash(rel))) {
			present = append(present, rel)
		}
	}
	if config.PyTruthy(manifest["provides_locales"]) && isDir(filepath.Join(pluginDir, "locales")) {
		present = append(present, "locales/ (language pack)")
	}
	if entry, ok := manifest["entry"]; ok && config.PyTruthy(entry) {
		present = append(present, "entry (runtime)")
	}
	if len(present) > 0 {
		report.add("loadable", true, "entry: "+strings.Join(present, ", "))
		return
	}
	report.add("loadable", false, "nothing to load: no runtime entry, desktop/plugin.js, plugin.json or provides_locales + locales/ beside plugin.yaml; "+PluginFormsText)
}

// Install time scanner at admission, dangerous fails and caution findings warn the reviewer
func checkSecurityScan(report *ValidationReport, pluginDir string) {
	result := ScanPlugin(pluginDir, "")
	set := map[string]bool{}
	for _, f := range result.Findings {
		if f.Severity == "critical" || f.Severity == "high" {
			set[fmt.Sprintf("%s (%s:%d)", f.PatternID, purePathName(f.File), f.Line)] = true
		}
	}
	summary := strings.Join(sortedKeys(set), ", ")
	if summary == "" {
		summary = "no findings"
	}
	if result.Verdict == "dangerous" {
		report.add("security scan", false, "dangerous: "+summary)
		return
	}
	report.add("security scan", true, result.Verdict)
	if result.Verdict == "caution" {
		report.warn("security scan caution: " + summary)
	}
}

// Final component of a path as pathlib names it, dot components and repeated separators dropped
func purePathName(p string) string {
	name := ""
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part != "" && part != "." {
			name = part
		}
	}
	return name
}

// Source order of the keys of the mapping a key path names in a YAML document
func yamlMappingKeyOrder(raw []byte, path ...string) []string {
	var doc yaml.Node
	if yaml.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	node := doc.Content[0]
	for _, key := range path {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				next = node.Content[i+1]
			}
		}
		if next == nil {
			return nil
		}
		node = next
	}
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	out := []string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, node.Content[i].Value)
	}
	return out
}

// Source order of the keys of the object a key path names in a JSON file
func jsonObjectKeyOrder(file string, path ...string) []string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	v, err := pyjson.LoadsBytes(stripBOM(raw))
	if err != nil {
		return nil
	}
	for _, key := range path {
		obj, ok := v.(*pyjson.Object)
		if !ok {
			return nil
		}
		if v, ok = obj.Get(key); !ok {
			return nil
		}
	}
	if obj, ok := v.(*pyjson.Object); ok {
		return append([]string{}, obj.Keys...)
	}
	return nil
}

// Runs every admission check against a plugin directory and returns the report
func validatePluginDir(ctx context.Context, path string) (*ValidationReport, error) {
	report, manifest, err := validateManifestAndProbe(ctx, path)
	if err != nil || manifest == nil {
		return report, err
	}
	if err := checkCoreOverride(report, path); err != nil {
		return nil, err
	}
	if err := checkPrivateImports(report, path); err != nil {
		return nil, err
	}
	checkDesktopSurface(report, path)
	checkLanguagePacks(report, manifest, path)
	isolation, err := auditPluginDir(path, manifest)
	if err != nil {
		return nil, err
	}
	report.Isolation = isolation
	return report, nil
}

// Manifest, dependency, probe and scan checks of a native plugin, a nil manifest when the report is final
func validateManifestAndProbe(ctx context.Context, path string) (*ValidationReport, map[string]any, error) {
	report := &ValidationReport{}
	if !isDir(path) {
		report.add("plugin directory", false, fmt.Sprintf("%s is not a directory", pypath.PurePathStr(path)))
		return report, nil, nil
	}
	manifestFile := filepath.Join(path, "plugin.yaml")
	if !isFile(manifestFile) {
		manifestFile = filepath.Join(path, "plugin.yml")
	}
	if !isFile(manifestFile) {
		if isFile(filepath.Join(path, "plugin.json")) {
			report, err := validatePortablePlugin(ctx, report, path)
			return report, nil, err
		}
		report.add("manifest", false, "no plugin.yaml (or portable plugin.json) in the plugin directory")
		return report, nil, nil
	}
	raw, err := os.ReadFile(manifestFile)
	if err == nil {
		err = strictUTF8(raw)
	}
	var doc any
	if err == nil {
		doc, err = config.FastSafeLoad(raw)
	}
	if err != nil {
		report.add("manifest", false, "plugin.yaml failed to parse: "+err.Error())
		return report, nil, nil
	}
	manifest, ok := doc.(map[string]any)
	if !ok {
		report.add("manifest", false, "plugin.yaml must be a mapping")
		return report, nil, nil
	}
	report.add("manifest", true, "plugin.yaml parses")
	checkManifestFields(report, manifest)
	checkRequiresIcebath(report, manifest)
	checkConfigSpec(report, manifest["config_schema"], yamlMappingKeyOrder(raw, "config_schema"))
	checkRequiresEnv(report, manifest)
	if err := checkOAuth(report, manifest, path); err != nil {
		return nil, nil, err
	}
	checkLoadable(report, path, manifest)
	checkPythonDependencies(report, path)
	checkPluginForm(report, manifest, path)
	recorded := checkCapabilities(ctx, report, manifest, path)
	checkBuiltinCollisionsWith(report, manifest, recorded)
	if err := checkTrustedInbound(report, recorded); err != nil {
		return nil, nil, err
	}
	checkSecurityScan(report, path)
	return report, manifest, nil
}

// Install time availability verdict of a portable server declaration on this host
type PortableAvailability struct {
	State   string
	Version string
	Path    string
}

// Admission checks of a portable plugin.json package, its reader diagnostics and server declarations
func validatePortablePlugin(ctx context.Context, report *ValidationReport, pluginDir string) (*ValidationReport, error) {
	dataRoot, err := os.MkdirTemp("", "")
	if err != nil {
		report.add("portable manifest", false, "plugin.json failed validation: "+pypath.OSErrorText(err))
		return report, nil
	}
	pkg, err := LoadAgentPlugin(pluginDir, dataRoot)
	os.RemoveAll(dataRoot)
	if err != nil {
		report.add("portable manifest", false, "plugin.json failed validation: "+err.Error())
		return report, nil
	}
	for _, diag := range pkg.Diagnostics {
		if diag.Scope != "" {
			report.Warnings = append(report.Warnings, diag.Scope+": "+diag.Message)
		} else {
			report.Warnings = append(report.Warnings, diag.Message)
		}
	}
	report.add("portable manifest", true, "plugin.json parses (Agent Plugins v1)")
	name := ""
	if config.PyTruthy(pkg.Manifest["name"]) {
		name = strings.TrimSpace(config.PyStr(pkg.Manifest["name"]))
	}
	if name != "" {
		report.add("manifest fields", true, "name present")
	} else {
		report.add("manifest fields", false, "plugin.json missing required 'name'")
	}
	manifestJSON := filepath.Join(pluginDir, "plugin.json")
	checkConfigSpec(report, manifestConfigSchema(pkg.Manifest), jsonObjectKeyOrder(manifestJSON, "extensions", portableExtension, "config_schema"))
	serverOrder := jsonObjectKeyOrder(filepath.Join(pluginDir, "mcp.json"), "mcpServers")
	servers := make(map[string]any, len(pkg.MCPServers))
	for k, v := range pkg.MCPServers {
		servers[k] = v
	}
	for _, serverName := range orderedMapKeys(servers, serverOrder) {
		cfg := pkg.MCPServers[serverName]
		if cfg["trust"] == "untrusted" {
			report.add("server trust: "+serverName, true, "untrusted (Icebath asks before every write-capable tool call)")
		}
		if costs := cfg["tool_costs"]; config.PyTruthy(costs) {
			limit := cfg["spend_limit"]
			if !config.PyTruthy(limit) {
				limit = 0
			}
			detail := fmt.Sprintf("%d costed tool(s), ", pyLen(costs))
			if config.PyTruthy(limit) {
				unit := "credits"
				if config.PyTruthy(cfg["cost_unit"]) {
					unit = config.PyStr(cfg["cost_unit"])
				}
				detail += fmt.Sprintf("spend limit %s %s per chat", config.PyStr(limit), unit)
			} else {
				detail += "no spend limit"
			}
			report.add("server costs: "+serverName, true, detail)
		}
	}
	declarations := make(map[string]any, len(pkg.ServerDeclarations))
	for k, v := range pkg.ServerDeclarations {
		declarations[k] = v
	}
	if len(declarations) > 0 {
		availability, err := needService("PortableServerAvailability", func(s *Services) func(context.Context, AgentPluginServerDeclaration) (PortableAvailability, error) {
			return s.PortableServerAvailability
		})
		if err != nil {
			return nil, err
		}
		for _, serverName := range orderedMapKeys(declarations, jsonObjectKeyOrder(manifestJSON, "extensions", portableExtension, "servers")) {
			result, err := availability(ctx, pkg.ServerDeclarations[serverName])
			if err != nil {
				return nil, err
			}
			detail := result.State
			if result.Version != "" {
				detail += ", version " + result.Version
			}
			if result.Path != "" {
				detail += ", path " + result.Path
			}
			report.add("server availability: "+serverName, true, detail)
		}
	}
	checkSecurityScan(report, pluginDir)
	checkDesktopSurface(report, pluginDir)
	isolation, err := auditPluginDir(pluginDir, pkg.Manifest)
	if err != nil {
		return nil, err
	}
	report.Isolation = isolation
	return report, nil
}

// Python len of a sized JSON value
func pyLen(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case map[string]any:
		return len(t)
	case string:
		return len([]rune(t))
	}
	return 0
}

// Reports the form a plugin takes, failing Python source and a runtime whose entry cannot be started
func checkPluginForm(report *ValidationReport, manifest map[string]any, pluginDir string) {
	mf := manifestFromData(manifest, pluginDir, "user", "")
	if manifestRuntime(mf) == "" {
		if isPythonPluginSource(mf) {
			report.add("plugin form", false, "the plugin is Python source; "+PluginFormsText)
			return
		}
		report.add("plugin form", true, "manifest only plugin, no runtime entry")
		return
	}
	spec, err := guestSpecOf(mf, filepath.Dir(filepath.Dir(pluginDir)))
	if err == nil && spec.runtime == RuntimeWasm {
		var entry string
		if entry, err = guestEntryPath(spec); err == nil && !isFile(entry) {
			err = fmt.Errorf("entry %s is not a file of the plugin directory", spec.entry[0])
		}
	}
	if err == nil && spec.runtime == RuntimeProgram {
		_, err = guestProgramPath(spec)
	}
	if err != nil {
		report.add("plugin form", false, err.Error())
		return
	}
	report.add("plugin form", true, "runtime "+spec.runtime+", entry "+strings.Join(spec.entry, " "))
}
