package dashboard

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Marker of a cleared field whose key falls back to the host or built in default
type memoryUnsetValue struct{}

var memoryUnset = memoryUnsetValue{}

// Python error of a given class a route helper raises where hermes raises it
type routePyError struct {
	class string
	msg   string
}

func (e *routePyError) Error() string { return e.msg }

// Python exception class name the error stands for
func (e *routePyError) PyTypeName() string { return e.class }

// Path object a provider module returned where the router expects plain data
type memoryPathValue struct{ text string }

// Native JSON value of a submitted string, the unset marker for blank number, json or text
func coerceDeclaredFieldValue(field memory.ProviderField, raw string) (any, error) {
	value := pystr.Strip(raw)
	switch field.Kind {
	case memory.KindSelect:
		if value == "" {
			value = field.Default
		}
		if !field.AllowedValues()[value] {
			return nil, &pypath.ValueError{Msg: fmt.Sprintf("Invalid value for '%s'", field.Key)}
		}
		return value, nil
	case memory.KindBool:
		return provider.IsTruthyValue(value), nil
	}
	if value == "" {
		return memoryUnset, nil
	}
	switch field.Kind {
	case memory.KindNumber:
		number, err := pyint.ParseFloat(value)
		if err != nil {
			return nil, &pypath.ValueError{Msg: fmt.Sprintf("Invalid number for '%s'", field.Key)}
		}
		if !math.IsInf(number, 0) && !math.IsNaN(number) && number == math.Trunc(number) {
			whole, err := pyint.Of(number)
			if err != nil {
				return nil, err
			}
			return pyint.Value(whole), nil
		}
		return number, nil
	case memory.KindJSON:
		parsed, err := pyjson.Loads(value)
		if err != nil {
			return nil, &pypath.ValueError{Msg: fmt.Sprintf("Invalid JSON for '%s'", field.Key)}
		}
		switch parsed.(type) {
		case *pyjson.Object, []any:
			return parsed, nil
		}
		return nil, &pypath.ValueError{Msg: fmt.Sprintf("'%s' must be a JSON object or array", field.Key)}
	}
	return value, nil
}

// JSON object of a file, empty when missing, unreadable or not an object
func readMemoryJSONDict(path, what string) *pyjson.Object {
	if _, err := os.Stat(path); err != nil {
		return pyjson.NewObject()
	}
	data, err := func() (any, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return nil, err
		}
		return pyjson.Loads(text)
	}()
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("Failed to read %s from %s", what, path), "exc", err)
		return pyjson.NewObject()
	}
	if obj, ok := data.(*pyjson.Object); ok && obj != nil {
		return obj
	}
	return pyjson.NewObject()
}

// Flat config file of a declared provider under the home in ctx
func memoryFlatJSONPath(ctx context.Context, schema *memory.ProviderConfigSchema) string {
	return filepath.Join(home.Dir(ctx), schema.Name, "config.json")
}

// Stored flat config of a declared provider
func readMemoryFlatJSON(ctx context.Context, schema *memory.ProviderConfigSchema) *pyjson.Object {
	return readMemoryJSONDict(memoryFlatJSONPath(ctx, schema), "memory provider config")
}

// Active host and config path the client module of a host block provider resolves
type honchoResolved struct {
	host       any
	hostPath   bool
	path       any
	pathIsPath bool
}

// JSON text and path positions of the arguments of a provider module call
func honchoArgs(values ...any) (string, []int, error) {
	parts := make([]string, len(values))
	var paths []int
	for i, v := range values {
		if p, ok := v.(memoryPathValue); ok {
			v = p.text
			paths = append(paths, i)
		}
		text, err := pyjson.Dumps(v)
		if err != nil {
			return "", nil, err
		}
		parts[i] = text
	}
	return "[" + strings.Join(parts, ", ") + "]", paths, nil
}

// Result of a provider module call as data, a returned path object kept as its marker
func honchoResult(out *plugins.ProviderModuleJSONResult) (args []any, result any, tracked []any, err error) {
	args, result, tracked, err = out.Graph()
	if err != nil {
		return nil, nil, nil, err
	}
	if out.ResultIsPath {
		result = memoryPathValue{text: out.ResultPath}
	}
	return args, result, tracked, nil
}

// Host block resolvers of the client module of a provider, its active host and config path resolved in that order
func honchoResolve(ctx context.Context, name string) (*honchoResolved, error) {
	require := []string{"resolve_active_host", "resolve_config_path", "_host_block"}
	out, err := memory.CallProviderModuleJSON(ctx, name, "client", "resolve_active_host", "[]", nil, require, nil)
	if err != nil {
		return nil, err
	}
	_, host, _, err := honchoResult(out)
	if err != nil {
		return nil, err
	}
	out, err = memory.CallProviderModuleJSON(ctx, name, "client", "resolve_config_path", "[]", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	_, path, _, err := honchoResult(out)
	if err != nil {
		return nil, err
	}
	_, hostPath := host.(memoryPathValue)
	_, pathIsPath := path.(memoryPathValue)
	return &honchoResolved{host: host, hostPath: hostPath, path: path, pathIsPath: pathIsPath}, nil
}

// Host block the client module resolves inside cfg, with cfg as the call left it and its tracked parts
func honchoHostBlock(ctx context.Context, name string, cfg any, host any, track [][]any) (cfgAfter any, block any, tracked []any, resultKeys [][]string, err error) {
	argsJSON, pathArgs, err := honchoArgs(cfg, host)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	out, err := memory.CallProviderModuleJSON(ctx, name, "client", "_host_block", argsJSON, pathArgs, nil, track)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	args, result, tracked, err := honchoResult(out)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return args[0], result, tracked, out.ResultKeys, nil
}

// Dict a Python attribute call needs, the AttributeError of any other value
func memoryDictOf(v any, attr string) (*pyjson.Object, error) {
	if obj, ok := v.(*pyjson.Object); ok && obj != nil {
		return obj, nil
	}
	return nil, &routePyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute '%s'", memoryTypeName(v), attr)}
}

// Python type name of a value of the declared config surface
func memoryTypeName(v any) string {
	if _, ok := v.(memoryPathValue); ok {
		return "PosixPath"
	}
	return pyrepr.TypeName(v)
}

// Python membership test of a key in a config source, the TypeError of a value that is no container
func memoryKeyIn(key string, source any) (bool, error) {
	switch t := source.(type) {
	case *pyjson.Object:
		if t != nil {
			_, ok := t.Get(key)
			return ok, nil
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == key {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(t, key), nil
	}
	return false, &routePyError{class: "TypeError", msg: fmt.Sprintf("argument of type '%s' is not iterable", memoryTypeName(source))}
}

// Key and aliases of a field in lookup order
func memoryFieldKeys(field memory.ProviderField) []string {
	return append([]string{field.Key}, field.Aliases...)
}

// One submitted secret of a declared provider
type memorySubmittedSecret struct {
	field     memory.ProviderField
	submitted string
}

// Persists each non blank secret with an env key to the env store and lists every non blank secret
func saveMemorySubmittedSecrets(ctx context.Context, schema *memory.ProviderConfigSchema, values *pyjson.Object) ([]memorySubmittedSecret, error) {
	var saved []memorySubmittedSecret
	for _, field := range schema.Fields {
		submitted := ""
		if field.IsSecret() {
			if s, ok := values.GetOr(field.Key, nil).(string); ok {
				submitted = pystr.Strip(s)
			}
		}
		if submitted == "" {
			continue
		}
		if field.EnvKey != "" {
			if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), field.EnvKey, submitted); err != nil {
				return nil, err
			}
		}
		saved = append(saved, memorySubmittedSecret{field: field, submitted: submitted})
	}
	return saved, nil
}

// Applies submitted non secret fields to their backend dict in place, a cleared field dropping its key and aliases
func applyMemoryFieldValues(schema *memory.ProviderConfigSchema, values *pyjson.Object, targetFor func(memory.ProviderField) any) error {
	for _, field := range schema.Fields {
		raw, present := values.Get(field.Key)
		if field.IsSecret() || !present {
			continue
		}
		target := targetFor(field)
		text, _ := raw.(string)
		coerced, err := coerceDeclaredFieldValue(field, text)
		if err != nil {
			return err
		}
		if _, unset := coerced.(memoryUnsetValue); unset {
			dict, err := memoryDictOf(target, "pop")
			if err != nil {
				return err
			}
			for _, key := range memoryFieldKeys(field) {
				dict.Delete(key)
			}
			continue
		}
		dict, ok := target.(*pyjson.Object)
		if !ok || dict == nil {
			return &routePyError{class: "TypeError", msg: fmt.Sprintf("'%s' object does not support item assignment", memoryTypeName(target))}
		}
		dict.Set(field.Key, coerced)
	}
	return nil
}

// Writes a private JSON file atomically, its directory created inside a live home
func writeMemoryJSON0600(ctx context.Context, path string, data any) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return config.AtomicJSONWriteMode(ctx, path, data, 0o600, pyjson.Indent(2))
}

// Saves submitted fields of a flat json provider into its config file
func writeMemoryProviderFlat(ctx context.Context, schema *memory.ProviderConfigSchema, values *pyjson.Object) error {
	existing := readMemoryFlatJSON(ctx, schema)
	if _, err := saveMemorySubmittedSecrets(ctx, schema, values); err != nil {
		return err
	}
	if err := applyMemoryFieldValues(schema, values, func(memory.ProviderField) any { return existing }); err != nil {
		return err
	}
	return writeMemoryJSON0600(ctx, memoryFlatJSONPath(ctx, schema), existing)
}

// Dict key a host value takes in the written JSON
func honchoHostKey(host any) string {
	switch t := host.(type) {
	case string:
		return t
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case memoryPathValue:
		return t.text
	}
	return pyrepr.Str(host)
}

// Whether a stored string starts with the access token prefix the oauth module declares
func honchoHasTokenPrefix(stored string, prefix any) (bool, error) {
	switch t := prefix.(type) {
	case string:
		return strings.HasPrefix(stored, t), nil
	case []any:
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return false, &routePyError{class: "TypeError", msg: fmt.Sprintf("tuple for startswith must only contain str, not %s", pyrepr.TypeName(item))}
			}
			if strings.HasPrefix(stored, s) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, &routePyError{class: "TypeError", msg: fmt.Sprintf("startswith first arg must be str or a tuple of str, not %s", pyrepr.TypeName(prefix))}
}

// Saves submitted fields into the host block config of the active host under the oauth locks of the provider
func writeMemoryProviderHoncho(ctx context.Context, schema *memory.ProviderConfigSchema, values *pyjson.Object) error {
	oauth, err := memory.ImportProviderModule(ctx, schema.Name, "oauth")
	if err != nil {
		return err
	}
	resolved, err := honchoResolve(ctx, schema.Name)
	if err != nil {
		return err
	}
	pathArgsJSON, pathArgs, err := honchoArgs(resolved.path)
	if err != nil {
		return err
	}
	hold, err := memory.HoldProviderModuleManagers(ctx, schema.Name, "oauth", []plugins.ProviderModuleManager{
		{Name: "_refresh_lock"},
		{Name: "_config_refresh_lock", Call: true, ArgsJSON: pathArgsJSON, PathArgs: pathArgs},
	})
	if err != nil {
		return err
	}
	bodyErr := func() error {
		out, err := memory.CallProviderModuleJSON(ctx, schema.Name, "oauth", "_read_config_strict", pathArgsJSON, pathArgs, nil, nil)
		if err != nil {
			return err
		}
		_, read, _, err := honchoResult(out)
		if err != nil {
			return err
		}
		cfg, err := memoryDictOf(read, "get")
		if err != nil {
			return err
		}
		hosts, isDict := cfg.GetOr("hosts", nil).(*pyjson.Object)
		if !isDict || hosts == nil {
			hosts = pyjson.NewObject()
		}
		cfg.Set("hosts", hosts)
		cfgAfter, existing, tracked, resultKeys, err := honchoHostBlock(ctx, schema.Name, cfg, resolved.host, [][]any{{int64(0), "hosts"}})
		if err != nil {
			return err
		}
		cfg, _ = cfgAfter.(*pyjson.Object)
		hosts, _ = tracked[0].(*pyjson.Object)
		hostKey := honchoHostKey(resolved.host)
		keyIsHost := true
		if memoryTruthy(existing) && len(resultKeys) > 0 && len(resultKeys[0]) > 0 {
			hostKey, keyIsHost = resultKeys[0][0], false
		}
		unwritableKey := keyIsHost && resolved.hostPath
		hostBlock, present := hosts.Get(hostKey)
		if unwritableKey {
			hostBlock = existing
		} else if !present {
			hosts.Set(hostKey, existing)
			hostBlock = existing
		}
		secrets, err := saveMemorySubmittedSecrets(ctx, schema, values)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			block, err := memoryDictOf(hostBlock, "get")
			if err != nil {
				return err
			}
			owned := false
			if stored, ok := block.GetOr(secret.field.Key, nil).(string); ok {
				prefix, err := oauth.Get(ctx, "ACCESS_TOKEN_PREFIX")
				if err != nil {
					return err
				}
				if owned, err = honchoHasTokenPrefix(stored, prefix); err != nil {
					return err
				}
			}
			if !owned {
				block.Set(secret.field.Key, secret.submitted)
			}
		}
		err = applyMemoryFieldValues(schema, values, func(field memory.ProviderField) any {
			if field.Scope == "host" {
				return hostBlock
			}
			return cfg
		})
		if err != nil {
			return err
		}
		path, isPath := resolved.path.(memoryPathValue)
		if !isPath {
			return &routePyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'parent'", memoryTypeName(resolved.path))}
		}
		if unwritableKey {
			if _, err := home.Mkdir(ctx, filepath.Dir(path.text)); err != nil {
				return err
			}
			return &routePyError{class: "TypeError", msg: "keys must be str, int, float, bool or None, not PosixPath"}
		}
		return writeMemoryJSON0600(ctx, path.text, cfg)
	}()
	class, message := "", ""
	if bodyErr != nil {
		class, message = pyrepr.ExceptionName(bodyErr), memoryExcStr(bodyErr)
		if memoryIsValueError(bodyErr) {
			class = "ValueError"
		}
	}
	suppressed, err := hold.Release(ctx, class, message)
	if err != nil {
		return err
	}
	if suppressed {
		return nil
	}
	return bodyErr
}

// Python truth of a value of the declared config surface, a path object always true
func memoryTruthy(v any) bool {
	if _, ok := v.(memoryPathValue); ok {
		return true
	}
	return pyrepr.Truthy(v)
}

// String the generic panel edits for a stored native value, the declared default for an absent key
func serializeMemoryFieldValue(field memory.ProviderField, value any) (string, error) {
	if value == nil {
		return field.Default, nil
	}
	if field.Kind == memory.KindBool {
		if provider.IsTruthyValue(value) {
			return "true", nil
		}
		return "false", nil
	}
	switch value.(type) {
	case *pyjson.Object, []any:
		if field.Kind == memory.KindJSON {
			return pyjson.Dumps(value)
		}
	}
	return pyrepr.Str(value), nil
}

// Stored native value of the first source holding the field, then its env fallbacks, nil when unset
func readMemoryField(field memory.ProviderField, sources []any, env map[string]string) (any, error) {
	for _, source := range sources {
		for _, key := range memoryFieldKeys(field) {
			held, err := memoryKeyIn(key, source)
			if err != nil {
				return nil, err
			}
			if !held {
				continue
			}
			dict, ok := source.(*pyjson.Object)
			if !ok {
				return nil, &routePyError{class: "TypeError", msg: memoryStrIndexError(source)}
			}
			if v := dict.GetOr(key, nil); v != nil {
				return v, nil
			}
		}
	}
	for _, envKey := range field.EnvFallbacks {
		if env[envKey] != "" {
			return env[envKey], nil
		}
	}
	return nil, nil
}

// Text of the TypeError a str index raises on a list or a str
func memoryStrIndexError(source any) string {
	if _, ok := source.(string); ok {
		return "string indices must be integers, not 'str'"
	}
	return "list indices must be integers or slices, not str"
}

// Whether a declared field has a value in the env store or any of its sources
func declaredMemoryFieldIsSet(field memory.ProviderField, sources []any, env map[string]string) (bool, error) {
	for _, envKey := range append([]string{field.EnvKey}, field.EnvFallbacks...) {
		if envKey != "" && env[envKey] != "" {
			return true, nil
		}
	}
	for _, source := range sources {
		dict, err := memoryDictOf(source, "get")
		if err != nil {
			return false, err
		}
		for _, key := range memoryFieldKeys(field) {
			if pyrepr.Truthy(dict.GetOr(key, nil)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Declared config surface of a provider with the stored value of every field, secrets write only
func declaredMemoryProviderPayload(ctx context.Context, schema *memory.ProviderConfigSchema) (*pyjson.Object, error) {
	env := webLoadEnv(ctx)
	isHoncho := schema.Storage == memory.StorageHonchoHostBlock
	var host any = ""
	var hostBlock, raw any
	var data *pyjson.Object
	if isHoncho {
		resolved, err := honchoResolve(ctx, schema.Name)
		if err != nil {
			return nil, err
		}
		host = resolved.host
		path, isPath := resolved.path.(memoryPathValue)
		if !isPath {
			return nil, &routePyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'exists'", memoryTypeName(resolved.path))}
		}
		raw, hostBlock, _, _, err = honchoHostBlock(ctx, schema.Name, readMemoryJSONDict(path.text, "Honcho config"), host, nil)
		if err != nil {
			return nil, err
		}
	} else {
		data = readMemoryFlatJSON(ctx, schema)
	}
	sourcesFor := func(field memory.ProviderField) []any {
		if !isHoncho {
			return []any{data}
		}
		if field.Scope == "host" {
			return []any{hostBlock, raw}
		}
		return []any{raw}
	}
	fields := []any{}
	for _, field := range schema.Fields {
		entry := pyjson.ObjectOf("key", field.Key, "label", field.Label, "kind", field.Kind, "description", field.Description,
			"info", field.Info, "placeholder", field.Placeholder, "inline", field.Inline, "group", field.Group)
		options := make([]any, 0, len(field.Options))
		for _, o := range field.Options {
			options = append(options, pyjson.ObjectOf("value", o.Value, "label", o.Label, "description", o.Description))
		}
		entry.Set("options", options)
		sources := sourcesFor(field)
		if field.IsSecret() {
			isSet, err := declaredMemoryFieldIsSet(field, sources, env)
			if err != nil {
				return nil, err
			}
			entry.Set("value", "")
			entry.Set("is_set", isSet)
			fields = append(fields, entry)
			continue
		}
		native, err := readMemoryField(field, sources, env)
		if err != nil {
			return nil, err
		}
		if isHoncho && field.Placeholder == "" && (field.Key == "workspace" || field.Key == "aiPeer") {
			if p, ok := host.(memoryPathValue); ok {
				entry.Set("placeholder", p.text)
			} else {
				entry.Set("placeholder", host)
			}
		}
		value, err := serializeMemoryFieldValue(field, native)
		if err != nil {
			return nil, err
		}
		if field.Kind == memory.KindSelect && !field.AllowedValues()[value] {
			value = field.Default
		}
		entry.Set("value", value)
		if isHoncho {
			entry.Set("is_set", native != nil)
		} else {
			entry.Set("is_set", value != "")
		}
		fields = append(fields, entry)
	}
	return pyjson.ObjectOf("name", schema.Name, "label", schema.Label, "docs_url", schema.DocsURL, "fields", fields), nil
}

// String form of a submitted value the declared schema path edits
func stringifyMemorySubmitted(value any) (string, error) {
	switch t := value.(type) {
	case nil:
		return "", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case *pyjson.Object, []any:
		return pyjson.Dumps(value)
	}
	return pyrepr.Str(value), nil
}

// The memory section of a config as a dict, a non dict value replaced by an empty one
func memorySection(cfg map[string]any) map[string]any {
	section, ok := cfg["memory"].(map[string]any)
	if !ok {
		section = map[string]any{}
		cfg["memory"] = section
	}
	return section
}

// Writes the submitted fields through the storage backend of the schema and makes the provider the active one
func updateMemoryProviderConfig(ctx context.Context, schema *memory.ProviderConfigSchema, values *pyjson.Object) error {
	writer := writeMemoryProviderFlat
	if schema.Storage == memory.StorageHonchoHostBlock {
		writer = writeMemoryProviderHoncho
	}
	if err := writer(ctx, schema, values); err != nil {
		return err
	}
	lctx, release := provider.LockConfigMutation(ctx)
	defer release()
	cfg, err := webLoadConfig(lctx)
	if err != nil {
		return err
	}
	section := memorySection(cfg)
	if current, ok := section["provider"].(string); !ok || current != schema.Name {
		section["provider"] = schema.Name
		return webSaveConfig(lctx, cfg, config.SaveOptions{})
	}
	return nil
}
