package dashboard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

var memoryProviderNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Dependency sync of a provider, the memory setup function hermes tests patch
var prepareMemoryProviderDependencies = memory.PrepareMemoryProviderDependencies

// 404 for a provider name nothing answers to
func unknownMemoryProvider(name string) error {
	return NewHTTPException(404, "Unknown memory provider: "+name)
}

// Whether err stands for a Python ValueError or one of its subclasses
func memoryIsValueError(err error) bool {
	switch pyrepr.ExceptionName(err) {
	case "ValueError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError", "JSONDecodeError", "EnvValueError":
		return true
	}
	if pe, ok := err.(*pyint.Error); ok {
		return pe.Kind == "ValueError"
	}
	return plugins.RemoteIsInstance(err, "ValueError")
}

// Python str of the exception an error stands for
func memoryExcStr(err error) string {
	return pyrepr.ExceptionMessage(err)
}

// A ValueError as a 400 with its text, any other failure a logged 500, an allowed HTTP error passing through
func memoryErrorsAsHTTP(logMsg, name string, passthroughHTTP bool, err error) error {
	if err == nil {
		return nil
	}
	if _, isHTTP := asHTTPException(err); isHTTP {
		if passthroughHTTP {
			return err
		}
		webServerLog().Error(fmt.Sprintf(logMsg, name), "exc", err)
		return NewHTTPException(500, "Internal server error")
	}
	if memoryIsValueError(err) {
		return NewHTTPException(400, memoryExcStr(err))
	}
	webServerLog().Error(fmt.Sprintf(logMsg, name), "exc", err)
	return NewHTTPException(500, "Internal server error")
}

// Setup output cut to a character limit with a truncation marker
func trimSetupOutput(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return pystr.Slice(text, 0, limit) + "\n... truncated ..."
}

// Result row of one setup step
func setupCommandResult(kind, name, status, command string, completed *setupCompleted, errText string) *pyjson.Object {
	var returncode any
	stdout, stderr := "", ""
	if completed != nil {
		returncode, stdout, stderr = int64(completed.Returncode), trimSetupOutput(completed.Stdout, 4000), completed.Stderr
	}
	if errText != "" {
		stderr = errText
	}
	return pyjson.ObjectOf("kind", kind, "name", name, "status", status, "command", command,
		"returncode", returncode, "stdout", stdout, "stderr", trimSetupOutput(stderr, 4000))
}

// Syncs the Python dependencies of a provider through pm, one result row when anything was prepared or failed
func installMemoryProviderPythonDependencies(ctx context.Context, name string) []any {
	command := "icebath pm install"
	_, status, err := prepareMemoryProviderDependencies(ctx, memoryToolsEnv(ctx), name)
	if err != nil {
		return []any{setupCommandResult("pip", name, "failed", command, nil, memoryExcStr(err))}
	}
	if status != "" {
		return []any{setupCommandResult("pip", name, status, command, nil, "")}
	}
	return []any{}
}

// Runs a setup command and appends its result row, the returncode or false when it could not run
func runSetupStep(ctx context.Context, results *[]any, kind, name, command string, statusOf func(ok bool) string, shell bool, timeoutSeconds int) (int, bool) {
	completed, err := func() (*setupCompleted, error) {
		if shell {
			return setupSubprocessRun(ctx, nil, command, true, timeoutSeconds)
		}
		argv, err := pyshlex.SplitPOSIX(command, false)
		if err != nil {
			return nil, err
		}
		return setupSubprocessRun(ctx, argv, command, false, timeoutSeconds)
	}()
	if err != nil {
		*results = append(*results, setupCommandResult(kind, name, statusOf(false), command, nil, memoryExcStr(err)))
		return 0, false
	}
	*results = append(*results, setupCommandResult(kind, name, statusOf(completed.Returncode == 0), command, completed, ""))
	return completed.Returncode, true
}

// Checks and installs each external dependency, a passing check skipping its install and a clean install checked again
func installMemoryProviderExternalDependencies(ctx context.Context, dependencies []any) []any {
	results := []any{}
	for _, raw := range dependencies {
		dep, _ := raw.(*pyjson.Object)
		text := func(key, def string) string {
			if v := dep.GetOr(key, nil); pyrepr.Truthy(v) {
				return pyrepr.Str(v)
			}
			return def
		}
		name, checkCmd, installCmd := text("name", "dependency"), text("check", ""), text("install", "")
		if checkCmd != "" {
			rc, ran := runSetupStep(ctx, &results, "external_check", name, checkCmd, func(ok bool) string {
				if ok {
					return "already_installed"
				}
				if installCmd != "" {
					return "missing"
				}
				return "failed"
			}, false, 20)
			if ran && rc == 0 {
				continue
			}
		}
		if installCmd == "" {
			continue
		}
		rc, ran := runSetupStep(ctx, &results, "external_install", name, installCmd, func(ok bool) string {
			if ok {
				return "installed"
			}
			return "failed"
		}, true, 300)
		if checkCmd != "" && ran && rc == 0 {
			runSetupStep(ctx, &results, "external_check", name, checkCmd, func(ok bool) string {
				if ok {
					return "verified"
				}
				return "failed"
			}, false, 20)
		}
	}
	return results
}

// Installs the declared dependencies of a provider and reports each step with the provider status afterwards
func installMemoryProviderSetup(ctx context.Context, name string) (*pyjson.Object, error) {
	loaded := loadDashboardMemoryProvider(ctx, name)
	manifest := memoryProviderManifest(ctx, name)
	if loaded == nil && len(manifest) == 0 {
		return nil, unknownMemoryProvider(name)
	}
	results := installMemoryProviderPythonDependencies(ctx, name)
	external := []any{}
	setup, _, err := memoryProviderSetupManifest(ctx, name)
	if err != nil {
		results = append(results, setupCommandResult("setup", name, "failed", "", nil, memoryExcStr(err)))
	} else {
		external, _ = setup.GetOr("external_dependencies", nil).([]any)
	}
	results = append(results, installMemoryProviderExternalDependencies(ctx, external)...)
	if len(results) == 0 {
		results = append(results, setupCommandResult("setup", name, "no_declared_steps", "", nil, ""))
	}
	ok := true
	for _, row := range results {
		if row.(*pyjson.Object).GetOr("status", nil) == "failed" {
			ok = false
		}
	}
	statuses, err := discoverMemoryProviderStatuses(ctx)
	if err != nil {
		return nil, err
	}
	var status any
	for _, row := range statuses {
		if row.GetOr("name", nil) == name {
			status = row
		}
	}
	return pyjson.ObjectOf("ok", ok, "provider", name, "results", results, "status", status), nil
}

// Legacy config surface of a loaded provider with stored values, secrets blank
func memoryProviderPayload(ctx context.Context, name string, loaded memory.Provider) (*pyjson.Object, error) {
	data := readMemoryProviderExistingValues(ctx, name)
	fields := []any{}
	for _, field := range normalizeMemoryProviderSchema(loaded) {
		entry := pyjson.NewObject()
		for _, k := range []string{"key", "label", "kind", "description", "placeholder", "required"} {
			entry.Set(k, field.GetOr(k, nil))
		}
		var value any = ""
		if field.GetOr("kind", nil) != "secret" {
			v, err := memoryFieldValue(ctx, field, data)
			if err != nil {
				return nil, err
			}
			value = v
		}
		isSet, err := memoryFieldIsSet(ctx, field, data)
		if err != nil {
			return nil, err
		}
		entry.Set("value", value)
		entry.Set("is_set", isSet)
		entry.Set("options", field.GetOr("options", []any{}))
		entry.Set("url", field.GetOr("url", ""))
		for _, k := range []string{"when", "minimum", "maximum", "step"} {
			entry.Set(k, field.GetOr(k, nil))
		}
		fields = append(fields, entry)
	}
	label := pystr.Title(strings.ReplaceAll(strings.ReplaceAll(name, "_", " "), "-", " "))
	return pyjson.ObjectOf("name", name, "label", label, "fields", fields, "setup", memoryProviderSetupInfo(ctx, name)), nil
}

// Exact value of a Python number for ordering, false for a value that is no number or is NaN
func memoryNumber(v any) (*big.Float, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(t)), true
	case int64:
		return new(big.Float).SetInt(big.NewInt(t)), true
	case *big.Int:
		return new(big.Float).SetInt(t), true
	case float64:
		if math.IsNaN(t) {
			return nil, true
		}
		return big.NewFloat(t), true
	}
	return nil, false
}

// Python ordering of two numbers, negative when a sorts first, a TypeError for a value that is no number
func memoryNumCompare(a, b any, op string) (int, bool, error) {
	x, okA := memoryNumber(a)
	y, okB := memoryNumber(b)
	if !okA || !okB {
		return 0, false, &routePyError{class: "TypeError", msg: fmt.Sprintf("'%s' not supported between instances of '%s' and '%s'", op, pyrepr.TypeName(a), pyrepr.TypeName(b))}
	}
	if x == nil || y == nil {
		return 0, false, nil
	}
	return x.Cmp(y), true, nil
}

// Finite number of a schema field, whole for an integer field and inside the declared bounds
func coerceSchemaNumber(field *pyjson.Object, raw any) (any, error) {
	key := pyrepr.Str(field.GetOr("key", nil))
	value := raw
	if raw == nil || raw == "" {
		def, err := memoryFieldDefault(field)
		if err != nil {
			return nil, err
		}
		value = def
	}
	invalid := &pypath.ValueError{Msg: fmt.Sprintf("Invalid numeric value for '%s'", key)}
	if _, isBool := value.(bool); isBool {
		return nil, invalid
	}
	number, err := pyint.Float(value)
	if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
		return nil, invalid
	}
	var result any = number
	if field.GetOr("kind", nil) == "integer" {
		if number != math.Trunc(number) {
			return nil, invalid
		}
		whole, err := pyint.Of(number)
		if err != nil {
			return nil, invalid
		}
		result = pyint.Value(whole)
	}
	if minimum := field.GetOr("minimum", nil); minimum != nil {
		cmp, ordered, err := memoryNumCompare(result, minimum, "<")
		if err != nil {
			return nil, err
		}
		if ordered && cmp < 0 {
			return nil, &pypath.ValueError{Msg: fmt.Sprintf("'%s' must be at least %s", key, pyrepr.Str(minimum))}
		}
	}
	if maximum := field.GetOr("maximum", nil); maximum != nil {
		cmp, ordered, err := memoryNumCompare(result, maximum, ">")
		if err != nil {
			return nil, err
		}
		if ordered && cmp > 0 {
			return nil, &pypath.ValueError{Msg: fmt.Sprintf("'%s' must be at most %s", key, pyrepr.Str(maximum))}
		}
	}
	return result, nil
}

// Native value of a submitted schema field by its kind, a blank text or select falling back to the default
func coerceSchemaField(field *pyjson.Object, raw any) (any, error) {
	kind := field.GetOr("kind", nil)
	if kind == "boolean" {
		def, err := memoryFieldDefault(field)
		if err != nil {
			return nil, err
		}
		fallback, err := coerceBool(def, false)
		if err != nil {
			return nil, err
		}
		return coerceBool(raw, fallback)
	}
	if kind == "integer" || kind == "number" {
		return coerceSchemaNumber(field, raw)
	}
	value := ""
	if raw != nil {
		value = pystr.Strip(pyrepr.Str(raw))
	}
	if kind == "select" {
		if value == "" {
			def, err := memoryFieldDefault(field)
			if err != nil {
				return nil, err
			}
			value = pyrepr.Str(def)
		}
		options, _ := field.GetOr("options", []any{}).([]any)
		for _, opt := range options {
			if o, ok := opt.(*pyjson.Object); ok && pyjson.Equal(o.GetOr("value", nil), value) {
				return value, nil
			}
		}
		return nil, &pypath.ValueError{Msg: fmt.Sprintf("Invalid value for '%s'", pyrepr.Str(field.GetOr("key", nil)))}
	}
	if value != "" {
		return value, nil
	}
	return memoryFieldDefault(field)
}

// Saves coerced values through the save_config of the provider class, else merges them into the memory section of config.yaml
func saveMemoryProviderNativeConfig(ctx context.Context, name string, loaded memory.Provider, values *pyjson.Object) error {
	if loaded != nil {
		saved, err := memory.SaveProviderNativeConfig(ctx, loaded, values, home.Dir(ctx))
		if err != nil || saved {
			return err
		}
	}
	lctx, release := provider.LockConfigMutation(ctx)
	defer release()
	cfg, err := webLoadConfig(lctx)
	if err != nil {
		return err
	}
	section := memorySection(cfg)
	merged := map[string]any{}
	if current, ok := section[name].(map[string]any); ok {
		for k, v := range current {
			merged[k] = v
		}
	}
	for _, k := range values.Keys {
		merged[k] = configMapOf(values.Vals[k])
	}
	section[name] = merged
	return webSaveConfig(lctx, cfg, config.SaveOptions{})
}

// Coerces the submitted values of every visible schema field, saves them and stores each submitted secret in the env store
func writeMemoryProviderConfigValues(ctx context.Context, name string, loaded memory.Provider, values *pyjson.Object) error {
	existing := readMemoryProviderExistingValues(ctx, name)
	fields := normalizeMemoryProviderSchema(loaded)
	byKey := map[string]*pyjson.Object{}
	for _, field := range fields {
		byKey[pyrepr.Str(field.GetOr("key", nil))] = field
	}
	configValues := pyjson.NewObject()
	secrets := pyjson.NewObject()
	for _, field := range fields {
		visibleIn := make(map[string]any, len(existing)+configValues.Len())
		for k, v := range existing {
			visibleIn[k] = v
		}
		for _, k := range configValues.Keys {
			visibleIn[k] = configValues.Vals[k]
		}
		visible, err := memoryFieldVisible(ctx, field, visibleIn, byKey)
		if err != nil {
			return err
		}
		if !visible {
			continue
		}
		key := pyrepr.Str(field.GetOr("key", nil))
		if field.GetOr("kind", nil) == "secret" {
			submitted := ""
			if v := values.GetOr(key, nil); pyrepr.Truthy(v) {
				submitted = pystr.Strip(pyrepr.Str(v))
			}
			if envKey := field.GetOr("_env_key", nil); submitted != "" && pyrepr.Truthy(envKey) {
				secrets.Set(pyrepr.Str(envKey), submitted)
			}
			continue
		}
		raw, submittedKey := values.Get(key)
		if !submittedKey {
			def, err := memoryFieldDefault(field)
			if err != nil {
				return err
			}
			if stored, ok := existing[key]; ok {
				raw = stored
			} else {
				raw = def
			}
		}
		coerced, err := coerceSchemaField(field, raw)
		if err != nil {
			return err
		}
		configValues.Set(key, coerced)
	}
	if err := saveMemoryProviderNativeConfig(ctx, name, loaded, configValues); err != nil {
		return err
	}
	for _, envKey := range secrets.Keys {
		if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), envKey, secrets.Vals[envKey].(string)); err != nil {
			return err
		}
	}
	return nil
}

// 404 for a provider name outside the strict charset that cannot leave the plugin dirs
func requireValidMemoryProviderName(name string) error {
	if !memoryProviderNameRe.MatchString(name) {
		return unknownMemoryProvider(name)
	}
	return nil
}

// Routes of hermes_cli/web_routers/memory_providers.py in declaration order
func memoryProvidersRouter() *APIRouter {
	r := NewAPIRouter("")
	namePath := PathParam("name", Str)
	optSurface := QueryParam("surface", Optional(Str), nil)
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/memory/providers/{name}/config", &Endpoint{
		Name:   "get_memory_provider_config",
		Params: []Param{namePath, optSurface, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			if err := requireValidMemoryProviderName(name); err != nil {
				return nil, err
			}
			surface, _ := c.Value("surface").(string)
			return scopedToThread(ctx, profileQuery(c), func(sctx context.Context) (any, error) {
				if surface == "declared" {
					declared := memory.GetProviderConfigSchema(sctx, memoryToolsEnv(sctx), name)
					if declared == nil {
						return pyjson.ObjectOf("name", name, "label", name, "docs_url", "", "fields", []any{}), nil
					}
					return declaredMemoryProviderPayload(sctx, declared)
				}
				loaded := loadDashboardMemoryProvider(sctx, name)
				if loaded == nil {
					return pyjson.ObjectOf("name", name, "label", name, "fields", []any{}, "setup", memoryProviderSetupInfo(sctx, name)), nil
				}
				return memoryProviderPayload(sctx, name, loaded)
			})
		},
	})
	r.Post("/api/memory/providers/{name}/setup", &Endpoint{
		Name:   "setup_memory_provider",
		Params: []Param{namePath, optProfile},
		Body:   []BodyParam{Body("body", memoryProviderSetupRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			if err := requireValidMemoryProviderName(name); err != nil {
				return nil, err
			}
			values, _ := c.Object("body").GetOr("values", nil).(*pyjson.Object)
			return scopedToThread(ctx, profileQuery(c), func(sctx context.Context) (any, error) {
				loaded := loadDashboardMemoryProvider(sctx, name)
				if loaded == nil && len(memoryProviderManifest(sctx, name)) == 0 {
					return nil, unknownMemoryProvider(name)
				}
				if loaded != nil && values != nil && values.Len() > 0 {
					err := writeMemoryProviderConfigValues(sctx, name, loaded, values)
					if err := memoryErrorsAsHTTP("Failed to persist memory provider setup values for %s", name, false, err); err != nil {
						return nil, err
					}
				}
				invalidatePluginsHubCache()
				return installMemoryProviderSetup(sctx, name)
			})
		},
	})
	r.Put("/api/memory/providers/{name}/config", &Endpoint{
		Name:   "update_memory_provider_config",
		Params: []Param{namePath, optSurface, optProfile},
		Body:   []BodyParam{Body("body", memoryProviderConfigUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			if err := requireValidMemoryProviderName(name); err != nil {
				return nil, err
			}
			values, _ := c.Object("body").GetOr("values", nil).(*pyjson.Object)
			if values == nil {
				values = pyjson.NewObject()
			}
			surface, _ := c.Value("surface").(string)
			result, err := scopedToThread(ctx, profileQuery(c), func(sctx context.Context) (any, error) {
				if surface == "declared" {
					declared := memory.GetProviderConfigSchema(sctx, memoryToolsEnv(sctx), name)
					if declared == nil {
						return nil, unknownMemoryProvider(name)
					}
					submitted := pyjson.NewObject()
					for _, k := range values.Keys {
						text, err := stringifyMemorySubmitted(values.Vals[k])
						if err != nil {
							return nil, err
						}
						submitted.Set(k, text)
					}
					if err := updateMemoryProviderConfig(sctx, declared, submitted); err != nil {
						return nil, err
					}
					invalidatePluginsHubCache()
					return pyjson.ObjectOf("ok", true), nil
				}
				loaded := loadDashboardMemoryProvider(sctx, name)
				if loaded == nil {
					return nil, unknownMemoryProvider(name)
				}
				if err := writeMemoryProviderConfigValues(sctx, name, loaded, values); err != nil {
					return nil, err
				}
				if err := requireMemoryProviderReady(sctx, name); err != nil {
					return nil, err
				}
				err := func() error {
					lctx, release := provider.LockConfigMutation(sctx)
					defer release()
					cfg, err := webLoadConfig(lctx)
					if err != nil {
						return err
					}
					memorySection(cfg)["provider"] = name
					return webSaveConfig(lctx, cfg, config.SaveOptions{})
				}()
				if err != nil {
					return nil, err
				}
				invalidatePluginsHubCache()
				return pyjson.ObjectOf("ok", true, "active", name), nil
			})
			if err := memoryErrorsAsHTTP("PUT /api/memory/providers/%s/config failed", name, true, err); err != nil {
				return nil, err
			}
			return result, nil
		},
	})
	return r
}
