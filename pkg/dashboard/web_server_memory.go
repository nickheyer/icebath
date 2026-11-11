package dashboard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool environment of the home in ctx for memory provider discovery
func memoryToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Memory provider by name, nil when unknown or failing to load
func loadDashboardMemoryProvider(ctx context.Context, name string) memory.Provider {
	return memory.LoadMemoryProvider(ctx, memoryToolsEnv(ctx), name)
}

// Plugin manifest of a memory provider, empty when it cannot be read
func memoryProviderManifest(ctx context.Context, name string) map[string]any {
	manifest, _, err := memory.MemoryProviderDependencyInputs(ctx, memoryToolsEnv(ctx), name)
	if err != nil {
		webServerLog().Debug(fmt.Sprintf("Failed to read memory provider manifest for %s", name), "exc", err)
		return map[string]any{}
	}
	return manifest
}

// Non blank stripped items of a list value, empty for anything else
func memoryStringList(value any) []any {
	list, ok := value.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, item := range list {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Setup display of a provider and the venv inputs its dependency readiness is probed with
func memoryProviderSetupManifest(ctx context.Context, name string) (*pyjson.Object, *pm.SyncVenvOptions, error) {
	env := memoryToolsEnv(ctx)
	manifest, inputs, err := memory.MemoryProviderDependencyInputs(ctx, env, name)
	if err != nil {
		return nil, nil, err
	}
	pluginDir := memory.FindProviderDir(ctx, env, name)
	external := []any{}
	if raw, ok := manifest["external_dependencies"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			dep := pyjson.NewObject()
			anySet := false
			for _, k := range []string{"name", "install", "check"} {
				v := pystr.Strip(pyrepr.Str(pyrepr.Or(m[k], "")))
				dep.Set(k, v)
				anySet = anySet || v != ""
			}
			if anySet {
				external = append(external, dep)
			}
		}
	}
	pipDeps := []any{}
	if pluginDir != "" {
		decl, err := pm.ReadPythonDeclaration(pluginDir)
		if err != nil {
			return nil, nil, err
		}
		reqs, err := decl.InstallRequirements()
		if err != nil {
			return nil, nil, err
		}
		pipDeps = stringsAny(reqs)
	}
	return pyjson.ObjectOf(
		"pip_dependencies", pipDeps,
		"python_dependencies_declared", inputs != nil,
		"external_dependencies", external,
		"required_env", memoryStringList(manifest["requires_env"]),
	), inputs, nil
}

// Setup display of a provider with whether its Python and external dependencies are installed
func memoryProviderSetupInfo(ctx context.Context, name string) *pyjson.Object {
	setup, inputs, err := memoryProviderSetupManifest(ctx, name)
	if err != nil {
		webServerLog().Debug(fmt.Sprintf("Invalid dependency declaration for %s", name), "exc", err)
		return pyjson.ObjectOf("pip_dependencies", []any{}, "python_dependencies_declared", true,
			"external_dependencies", []any{}, "required_env", []any{}, "dependencies_installed", false)
	}
	pythonReady := true
	if inputs != nil {
		current, err := pm.VenvIsCurrent(ctx, pm.VenvIsCurrentOptions{ProjectRoot: inputs.ProjectRoot, Extras: inputs.Extras, Plugins: inputs.Plugins})
		if err != nil {
			webServerLog().Debug(fmt.Sprintf("Could not read dependency state for %s", name), "exc", err)
			current = false
		}
		pythonReady = current
	}
	setup.Set("dependencies_installed", pythonReady && memoryProviderExternalDependenciesInstalled(ctx, setup))
	return setup
}

// Unscrubbed child environment with the common user tool bin dirs ahead of PATH
func memoryProviderSetupEnv(ctx context.Context) ([]string, error) {
	env, err := tools.BuildSubprocessEnv(ctx, memoryToolsEnv(ctx), tools.SubprocessEnvOptions{ScrubSecrets: false, InheritProfileHome: false})
	if err != nil {
		return nil, err
	}
	userHome, err := pypath.PathHome()
	if err != nil {
		return nil, err
	}
	var prefix []string
	for _, p := range []string{filepath.Join(userHome, ".brv-cli", "bin"), filepath.Join(userHome, ".local", "bin"), filepath.Join(userHome, ".npm-global", "bin"), "/usr/local/bin"} {
		if _, err := os.Stat(p); err == nil {
			prefix = append(prefix, p)
		}
	}
	if len(prefix) > 0 {
		env["PATH"] = strings.Join(prefix, string(os.PathListSeparator)) + string(os.PathListSeparator) + env["PATH"]
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out, nil
}

// Exit code of a setup command run with the setup environment, an error when it cannot start or times out
func runSetupCommand(ctx context.Context, argv []string, timeout time.Duration) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("empty command")
	}
	env, err := memoryProviderSetupEnv(ctx)
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && cctx.Err() == nil {
			return exitErr.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}

// Whether every external dependency check passes, an install without a check counting as missing
func memoryProviderExternalDependenciesInstalled(ctx context.Context, setup *pyjson.Object) bool {
	externalOK := true
	deps, _ := setup.GetOr("external_dependencies", nil).([]any)
	for _, raw := range deps {
		dep, ok := raw.(*pyjson.Object)
		if !ok {
			continue
		}
		check := pystr.Strip(pyrepr.Str(pyrepr.Or(dep.GetOr("check", nil), "")))
		if check == "" {
			if pystr.Strip(pyrepr.Str(pyrepr.Or(dep.GetOr("install", nil), ""))) != "" {
				externalOK = false
			}
			continue
		}
		argv, err := pyshlex.SplitPOSIX(check, true)
		if err != nil {
			externalOK = false
			continue
		}
		code, err := runSetupCommand(ctx, argv, 20*time.Second)
		if err != nil || code != 0 {
			externalOK = false
		}
	}
	return externalOK
}

// Field kind from an explicit kind or type hint, else inferred from the default
func schemaFieldKind(raw map[string]any, choices []any) string {
	explicit := pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(raw["kind"], raw["type"]), ""))))
	def := raw["default"]
	if pyrepr.Truthy(raw["secret"]) {
		return "secret"
	}
	if len(choices) > 0 {
		return "select"
	}
	_, isBool := def.(bool)
	if explicit == "bool" || explicit == "boolean" || isBool {
		return "boolean"
	}
	switch def.(type) {
	case int, int64:
		return "integer"
	}
	if explicit == "int" || explicit == "integer" {
		return "integer"
	}
	_, isFloat := def.(float64)
	if explicit == "float" || explicit == "number" || isFloat {
		return "number"
	}
	return "text"
}

// Dashboard setup fields of a provider config schema, keyless fields dropped
func normalizeMemoryProviderSchema(provider memory.Provider) []*pyjson.Object {
	fields := []*pyjson.Object{}
	if provider == nil {
		return fields
	}
	for _, raw := range provider.ConfigSchema() {
		if raw == nil {
			continue
		}
		key := pystr.Strip(pyrepr.Str(pyrepr.Or(raw["key"], "")))
		if key == "" {
			continue
		}
		choicesRaw := pyrepr.Or(pyrepr.Or(raw["choices"], raw["options"]), []any{})
		choices, ok := choicesRaw.([]any)
		if !ok {
			choices = []any{}
		}
		label := pyrepr.Str(pyrepr.Or(raw["label"], pystr.Title(strings.ReplaceAll(key, "_", " "))))
		def, hasDefault := raw["default"]
		if !hasDefault {
			def = ""
		}
		options := make([]any, 0, len(choices))
		for _, ch := range choices {
			options = append(options, pyjson.ObjectOf("value", pyrepr.Str(ch), "label", pyrepr.Str(ch), "description", ""))
		}
		var when any
		if w, ok := raw["when"].(map[string]any); ok {
			when = w
		}
		var envKey any
		if e := pyrepr.Str(pyrepr.Or(raw["env_var"], "")); e != "" {
			envKey = e
		}
		fields = append(fields, pyjson.ObjectOf(
			"key", key,
			"label", label,
			"kind", schemaFieldKind(raw, choices),
			"description", pyrepr.Str(pyrepr.Or(raw["description"], "")),
			"placeholder", pyrepr.Str(pyrepr.Or(raw["placeholder"], "")),
			"required", pyrepr.Truthy(raw["required"]),
			"default", def,
			"options", options,
			"url", pyrepr.Str(pyrepr.Or(raw["url"], "")),
			"when", when,
			"minimum", raw["minimum"],
			"maximum", raw["maximum"],
			"step", raw["step"],
			"_env_key", envKey,
		))
	}
	return fields
}

// JSON object of a file, empty when missing, unreadable or not an object
func readMemoryJSONFile(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return map[string]any{}
	}
	obj, err := pyjson.LoadsDict(text)
	if err != nil {
		webServerLog().Debug(fmt.Sprintf("Failed to read JSON config from %s", path), "exc", err)
		return map[string]any{}
	}
	return configMapOf(obj).(map[string]any)
}

// Existing provider config merged from the legacy and native JSON stores and config.yaml
func readMemoryProviderExistingValues(ctx context.Context, name string) map[string]any {
	h := home.Dir(ctx)
	values := map[string]any{}
	for _, path := range []string{filepath.Join(h, name+".json"), filepath.Join(h, name, "config.json")} {
		for k, v := range readMemoryJSONFile(path) {
			values[k] = v
		}
	}
	cfg, err := webLoadConfig(ctx)
	if err != nil || cfg == nil {
		cfg = map[string]any{}
	}
	if memoryCfg, ok := cfg["memory"].(map[string]any); ok {
		if providerCfg, ok := memoryCfg[name].(map[string]any); ok {
			for k, v := range providerCfg {
				values[k] = v
			}
		}
		if legacy, ok := memoryCfg["provider_config"].(map[string]any); ok {
			merged := map[string]any{}
			for k, v := range legacy {
				merged[k] = v
			}
			for k, v := range values {
				merged[k] = v
			}
			values = merged
		}
	}
	if pluginsCfg, ok := cfg["plugins"].(map[string]any); ok && name == "holographic" {
		if holo, ok := pluginsCfg["hermes-memory-store"].(map[string]any); ok {
			for k, v := range holo {
				values[k] = v
			}
		}
	}
	return values
}

// Env value of the home .env, then the process env, empty for no key
func memoryEnvLookup(ctx context.Context, envKey any) string {
	key, _ := envKey.(string)
	if key == "" {
		return ""
	}
	if v := config.LoadEnv(home.Dir(ctx))[key]; v != "" {
		return v
	}
	return os.Getenv(key)
}

// Boolean of a config value, the default for None or empty, an error for unrecognized text
func coerceBool(value any, def bool) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case nil:
		return def, nil
	case string:
		if v == "" {
			return def, nil
		}
	case int, int64, float64:
		return pyrepr.Truthy(v), nil
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(value))) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("Invalid boolean value: %s", pyrepr.Str(value))
}

// Default of a field, coerced to a boolean for boolean fields
func memoryFieldDefault(field *pyjson.Object) (any, error) {
	def := field.GetOr("default", "")
	if field.GetOr("kind", nil) == "boolean" {
		return coerceBool(def, false)
	}
	return def, nil
}

// Whether a value is None or the empty string
func noneOrEmpty(v any) bool { return v == nil || v == "" }

// Display value of a field from the stored data, env and default, secrets always blank
func memoryFieldValue(ctx context.Context, field *pyjson.Object, data map[string]any) (any, error) {
	kind := field.GetOr("kind", nil)
	if kind == "secret" {
		return "", nil
	}
	value := data[pyrepr.Str(field.GetOr("key", nil))]
	if noneOrEmpty(value) {
		value = memoryEnvLookup(ctx, field.GetOr("_env_key", nil))
	}
	if noneOrEmpty(value) {
		def, err := memoryFieldDefault(field)
		if err != nil {
			return nil, err
		}
		value = def
	}
	switch kind {
	case "select":
		allowed := map[string]bool{}
		opts, _ := field.GetOr("options", nil).([]any)
		for _, o := range opts {
			if obj, ok := o.(*pyjson.Object); ok {
				allowed[pyrepr.Str(obj.GetOr("value", nil))] = true
			}
		}
		text := pyrepr.Str(value)
		if allowed[text] {
			return text, nil
		}
		def, err := memoryFieldDefault(field)
		if err != nil {
			return nil, err
		}
		return pyrepr.Str(def), nil
	case "boolean":
		def, err := memoryFieldDefault(field)
		if err != nil {
			return nil, err
		}
		fallback, err := coerceBool(def, false)
		if err != nil {
			return nil, err
		}
		return coerceBool(value, fallback)
	}
	return pyrepr.Str(value), nil
}

// Whether a field has a value, a secret counting when its env key or stored value is set
func memoryFieldIsSet(ctx context.Context, field *pyjson.Object, data map[string]any) (bool, error) {
	if field.GetOr("kind", nil) == "secret" {
		return memoryEnvLookup(ctx, field.GetOr("_env_key", nil)) != "" || pyrepr.Truthy(data[pyrepr.Str(field.GetOr("key", nil))]), nil
	}
	v, err := memoryFieldValue(ctx, field, data)
	if err != nil {
		return false, err
	}
	return !noneOrEmpty(v), nil
}

// Whether every when dependency of a field holds its expected value
func memoryFieldVisible(ctx context.Context, field *pyjson.Object, data map[string]any, byKey map[string]*pyjson.Object) (bool, error) {
	when, ok := field.GetOr("when", nil).(map[string]any)
	if !ok || len(when) == 0 {
		return true, nil
	}
	for _, depKey := range sortedMapKeys(when) {
		dep := byKey[depKey]
		if dep == nil {
			dep = pyjson.ObjectOf("key", depKey, "kind", "text", "default", "", "_env_key", nil)
		}
		v, err := memoryFieldValue(ctx, dep, data)
		if err != nil {
			return false, err
		}
		if pyrepr.Str(v) != pyrepr.Str(when[depKey]) {
			return false, nil
		}
	}
	return true, nil
}

// Whether every required visible field of the provider schema has a value
func memoryProviderIsConfigured(ctx context.Context, name string, provider memory.Provider) (bool, error) {
	data := readMemoryProviderExistingValues(ctx, name)
	fields := normalizeMemoryProviderSchema(provider)
	byKey := map[string]*pyjson.Object{}
	for _, f := range fields {
		byKey[pyrepr.Str(f.GetOr("key", nil))] = f
	}
	for _, f := range fields {
		if !pyrepr.Truthy(f.GetOr("required", nil)) {
			continue
		}
		visible, err := memoryFieldVisible(ctx, f, data, byKey)
		if err != nil {
			return false, err
		}
		if !visible {
			continue
		}
		set, err := memoryFieldIsSet(ctx, f, data)
		if err != nil {
			return false, err
		}
		if !set {
			return false, nil
		}
	}
	return true, nil
}

// Status of a provider row, missing, unavailable, needs_config or ready
func memoryProviderStatus(ctx context.Context, row *pyjson.Object, setup *pyjson.Object, configured bool, schemaFields []*pyjson.Object) string {
	if pyrepr.Truthy(row.GetOr("missing", nil)) {
		return "missing"
	}
	if _, parked := memory.ParkedProviderNotice(ctx, memoryToolsEnv(ctx), pyrepr.Str(row.GetOr("name", nil))); parked {
		return "unavailable"
	}
	if !pyrepr.Truthy(setup.GetOr("dependencies_installed", true)) {
		return "unavailable"
	}
	available := pyrepr.Truthy(row.GetOr("available", nil))
	if !configured || (!available && len(schemaFields) > 0) {
		return "needs_config"
	}
	if available {
		return "ready"
	}
	return "unavailable"
}

// Every discovered provider plus a missing configured one, sorted by name with status and setup
func discoverMemoryProviderStatuses(ctx context.Context) ([]*pyjson.Object, error) {
	discovered := map[string]*pyjson.Object{}
	for _, d := range memory.DiscoverMemoryProviders(ctx, memoryToolsEnv(ctx)) {
		discovered[d.Name] = pyjson.ObjectOf("name", d.Name, "description", d.Description, "available", d.Available, "missing", false)
	}
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	active := ""
	if mem, ok := cfg["memory"].(map[string]any); ok {
		active = normalizeMemoryProviderName(mem["provider"])
	}
	if _, seen := discovered[active]; active != "" && !seen {
		discovered[active] = pyjson.ObjectOf("name", active, "description", "Configured provider was not found.", "available", false, "missing", true)
	}
	names := make([]string, 0, len(discovered))
	for n := range discovered {
		names = append(names, n)
	}
	slices.Sort(names)
	providers := make([]*pyjson.Object, 0, len(names))
	for _, name := range names {
		row := discovered[name]
		missing := pyrepr.Truthy(row.GetOr("missing", nil))
		var provider memory.Provider
		if !missing {
			provider = loadDashboardMemoryProvider(ctx, name)
		}
		setup := memoryProviderSetupInfo(ctx, name)
		configured := false
		schemaFields := []*pyjson.Object{}
		if !missing {
			if configured, err = memoryProviderIsConfigured(ctx, name, provider); err != nil {
				return nil, err
			}
			schemaFields = normalizeMemoryProviderSchema(provider)
		}
		providers = append(providers, pyjson.ObjectOf(
			"name", name,
			"description", row.GetOr("description", nil),
			"available", row.GetOr("available", nil),
			"configured", configured,
			"status", memoryProviderStatus(ctx, row, setup, configured, schemaFields),
			"setup", setup,
		))
	}
	return providers, nil
}

// 400 unless the named provider is discovered and ready, nothing for the built in choice
func requireMemoryProviderReady(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	rows, err := discoverMemoryProviderStatuses(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.GetOr("name", nil) != name {
			continue
		}
		status := pyrepr.Str(r.GetOr("status", nil))
		if status != "ready" {
			return NewHTTPException(400, fmt.Sprintf("Memory provider '%s' is not ready (%s). Configure it in the dashboard first.", name, strings.ReplaceAll(status, "_", " ")))
		}
		return nil
	}
	return NewHTTPException(400, fmt.Sprintf("Unknown memory provider '%s'.", name))
}
