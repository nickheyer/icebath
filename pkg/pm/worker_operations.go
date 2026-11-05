package pm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Prerequisite policies of worker operations, always, policy, state and never
const (
	bootstrapAlways = "always"
	bootstrapPolicy = "policy"
	bootstrapState  = "state"
	bootstrapNever  = "never"
)

// Callbacks and cancellation the worker hands an operation, each also a keyword the operation must accept
type workerHooks struct {
	pause            *PauseEvent
	progress         InstallProgress
	downloadProgress ProgressFunc
}

// One routable worker operation, the packages it needs restored, its prerequisite policy and its dispatch
type workerOperation struct {
	packages  []string
	bootstrap string
	run       func(ctx context.Context, args map[string]any, hooks workerHooks) (any, error)
}

// Keyword arguments of one dispatched call with Python's binding errors
type kwargs struct {
	op         string
	args       map[string]any
	positional []string
	keyword    []string
}

// Binding of an operation's keyword arguments, hook keywords included as the worker adds them
func bindKwargs(op string, args map[string]any, hooks workerHooks, positional, keyword []string) *kwargs {
	bound := make(map[string]any, len(args)+3)
	for k, v := range args {
		bound[k] = v
	}
	if hooks.pause != nil {
		bound["pause_event"] = hooks.pause
	}
	if hooks.progress != nil {
		bound["progress"] = hooks.progress
	}
	if hooks.downloadProgress != nil {
		bound["download_progress"] = hooks.downloadProgress
	}
	return &kwargs{op: op, args: bound, positional: positional, keyword: keyword}
}

// Python wording of a missing argument list, quoted and joined with and
func missingList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}

// Python TypeError for required arguments the call left out, positional ones reported first
func (k *kwargs) checkRequired() error {
	for _, group := range []struct {
		names []string
		kind  string
	}{{k.positional, "positional"}, {k.keyword, "keyword-only"}} {
		var missing []string
		for _, name := range group.names {
			if _, ok := k.args[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		plural := ""
		if len(missing) > 1 {
			plural = "s"
		}
		return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s() missing %d required %s argument%s: %s",
			k.op, len(missing), group.kind, plural, missingList(missing))}
	}
	return nil
}

// Python TypeError for the first keyword the operation does not accept
func (k *kwargs) checkKnown(known map[string]bool) error {
	names := make([]string, 0, len(k.args))
	for name := range k.args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !known[name] {
			return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s() got an unexpected keyword argument '%s'", k.op, name)}
		}
	}
	return nil
}

// Snake case keyword a Go field name stands for, initialisms kept as one word
func camelToSnake(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prevLower := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || (nextLower && unicode.IsUpper(runes[i-1])) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Python TypeError for a value of the wrong JSON kind
func kwargTypeError(op, name, want string, value any) error {
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s() argument '%s' must be %s, not %s", op, name, want, pyTypeName(value))}
}

// Assigns one decoded JSON value to a field of the options struct
func assignKwarg(op, name string, field reflect.Value, value any) error {
	switch field.Type() {
	case reflect.TypeOf(time.Duration(0)):
		seconds, err := jsonNumber(value)
		if err != nil {
			return kwargTypeError(op, name, "int", value)
		}
		field.SetInt(int64(seconds * float64(time.Second)))
		return nil
	case reflect.TypeOf(""):
		if value == nil {
			field.SetString("")
			return nil
		}
		s, ok := value.(string)
		if !ok {
			return kwargTypeError(op, name, "str", value)
		}
		field.SetString(s)
		return nil
	case reflect.TypeOf(false):
		field.SetBool(truthy(value))
		return nil
	case reflect.TypeOf([]string(nil)):
		if value == nil {
			field.Set(reflect.Zero(field.Type()))
			return nil
		}
		list, err := pyIterStrings(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(list))
		return nil
	case reflect.TypeOf(map[string]string(nil)):
		if value == nil {
			field.Set(reflect.Zero(field.Type()))
			return nil
		}
		object, ok := value.(map[string]any)
		if !ok {
			return kwargTypeError(op, name, "a mapping", value)
		}
		out := make(map[string]string, len(object))
		for k, v := range object {
			s, ok := v.(string)
			if !ok {
				return kwargTypeError(op, name, "a mapping of str", v)
			}
			out[k] = s
		}
		field.Set(reflect.ValueOf(out))
		return nil
	}
	if field.Kind() == reflect.Int {
		n, err := jsonNumber(value)
		if err != nil {
			return kwargTypeError(op, name, "int", value)
		}
		field.SetInt(int64(n))
		return nil
	}
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s() argument '%s' has no wire form", op, name)}
}

// Number of a decoded JSON value
func jsonNumber(value any) (float64, error) {
	switch n := value.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case json.Number:
		return strconv.ParseFloat(n.String(), 64)
	}
	return 0, fmt.Errorf("not a number")
}

// Decodes keyword arguments into an options struct whose fields name the Python keywords
func (k *kwargs) decodeInto(out any) error {
	if err := k.checkRequired(); err != nil {
		return err
	}
	v := reflect.ValueOf(out).Elem()
	fields := map[string]int{}
	known := map[string]bool{}
	for i := 0; i < v.NumField(); i++ {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		key := camelToSnake(v.Type().Field(i).Name)
		fields[key] = i
		known[key] = true
	}
	if err := k.checkKnown(known); err != nil {
		return err
	}
	for name, value := range k.args {
		if err := assignKwarg(k.op, name, v.Field(fields[name]), value); err != nil {
			return err
		}
	}
	return nil
}

// Operation returning a path, its options decoded from the request keywords
func pathOperation[T any](op string, positional, keyword []string, call func(context.Context, T) (string, error)) func(context.Context, map[string]any, workerHooks) (any, error) {
	return func(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
		var opts T
		if err := bindKwargs(op, args, hooks, positional, keyword).decodeInto(&opts); err != nil {
			return nil, err
		}
		path, err := call(ctx, opts)
		if err != nil {
			return nil, err
		}
		return path, nil
	}
}

// Operation returning nothing, its options decoded from the request keywords
func voidOperation[T any](op string, positional, keyword []string, call func(context.Context, T) error) func(context.Context, map[string]any, workerHooks) (any, error) {
	return func(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
		var opts T
		if err := bindKwargs(op, args, hooks, positional, keyword).decodeInto(&opts); err != nil {
			return nil, err
		}
		return nil, call(ctx, opts)
	}
}

// Value of an optional keyword and whether the call passed it
func (k *kwargs) take(name string) (any, bool) {
	v, ok := k.args[name]
	return v, ok
}

// String keyword, a TypeError for another kind
func (k *kwargs) str(name string) (string, error) {
	v, _ := k.take(name)
	s, ok := v.(string)
	if !ok {
		return "", kwargTypeError(k.op, name, "str", v)
	}
	return s, nil
}

// Optional string list keyword, nil for an absent or null value
func (k *kwargs) list(name string) ([]string, error) {
	v, ok := k.take(name)
	if !ok || v == nil {
		return nil, nil
	}
	return pyIterStrings(v)
}

// Boolean keyword with its Python default
func (k *kwargs) flag(name string, fallback bool) bool {
	v, ok := k.take(name)
	if !ok {
		return fallback
	}
	return truthy(v)
}

// Plugin input keyword decoded from its wire form, nil when absent or null
func (k *kwargs) plugins() (PluginInput, error) {
	raw, ok := k.take("plugins")
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, isMap := raw.(map[string]any)
	if !isMap {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(raw))}
	}
	return DecodePluginInput(encoded)
}

// Keyword set of an engine function
func keywordSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Worker dispatch of the tool install engine
func runEnsure(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
	k := bindKwargs("ensure", args, hooks, []string{"name"}, nil)
	if err := k.checkRequired(); err != nil {
		return nil, err
	}
	if err := k.checkKnown(keywordSet("name", "base_env", "explicit", "verify", "progress", "pause_event", "download_progress")); err != nil {
		return nil, err
	}
	name, err := k.str("name")
	if err != nil {
		return nil, err
	}
	opts := EnsureOptions{Explicit: k.flag("explicit", false), SkipVerify: !k.flag("verify", true),
		Progress: hooks.progress, PauseEvent: hooks.pause, DownloadProgress: hooks.downloadProgress}
	if base, ok := k.take("base_env"); ok && base != nil {
		object, isMap := base.(map[string]any)
		if !isMap {
			return nil, kwargTypeError("ensure", "base_env", "a mapping", base)
		}
		opts.BaseEnv = map[string]string{}
		for key, value := range object {
			opts.BaseEnv[key] = pyStr(value)
		}
	}
	if _, err := ensure(ctx, name, opts); err != nil {
		return nil, err
	}
	return nil, nil
}

// Worker dispatch of a cross target stage
func runStageOnly(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
	k := bindKwargs("stage_only", args, hooks, []string{"name", "target"}, nil)
	if err := k.checkRequired(); err != nil {
		return nil, err
	}
	if err := k.checkKnown(keywordSet("name", "target", "progress", "pause_event", "download_progress")); err != nil {
		return nil, err
	}
	name, err := k.str("name")
	if err != nil {
		return nil, err
	}
	target, err := k.str("target")
	if err != nil {
		return nil, err
	}
	return stageOnly(ctx, name, target, StageOnlyOptions{Progress: hooks.progress, PauseEvent: hooks.pause, DownloadProgress: hooks.downloadProgress})
}

// Worker dispatch of a venv sync with its plugin input decoded
func runSyncVenv(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
	k := bindKwargs("sync_venv", args, hooks, nil, nil)
	if err := k.checkKnown(keywordSet("extras", "explicit", "plugins", "repair", "evict_incompatible_plugins", "automatic", "carried")); err != nil {
		return nil, err
	}
	opts := SyncVenvOptions{Explicit: k.flag("explicit", false), Repair: k.flag("repair", false),
		EvictIncompatiblePlugins: k.flag("evict_incompatible_plugins", false)}
	var err error
	if opts.Extras, err = k.list("extras"); err != nil {
		return nil, err
	}
	if opts.Automatic, err = k.list("automatic"); err != nil {
		return nil, err
	}
	if opts.Carried, err = k.list("carried"); err != nil {
		return nil, err
	}
	if opts.Plugins, err = k.plugins(); err != nil {
		return nil, err
	}
	return nil, syncVenv(ctx, opts)
}

// Worker dispatch of a currency probe with its plugin input decoded
func runVenvIsCurrent(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
	k := bindKwargs("venv_is_current", args, hooks, nil, nil)
	if err := k.checkKnown(keywordSet("extras", "plugins", "project_root")); err != nil {
		return nil, err
	}
	extras, err := k.list("extras")
	if err != nil {
		return nil, err
	}
	plugins, err := k.plugins()
	if err != nil {
		return nil, err
	}
	root := ""
	if v, ok := k.take("project_root"); ok && v != nil {
		if root, err = k.str("project_root"); err != nil {
			return nil, err
		}
	}
	return venvIsCurrent(ctx, extras, plugins, root)
}

// Every operation the private worker routes, by its Python name
var workerOperations = map[string]*workerOperation{
	"ensure":          {bootstrap: bootstrapState, run: runEnsure},
	"stage_only":      {bootstrap: bootstrapAlways, run: runStageOnly},
	"stage_tools":     {bootstrap: bootstrapNever, run: pathOperation("stage_tools", []string{"names"}, []string{"source_store", "out", "target"}, stageTools)},
	"prepare_tools":   {bootstrap: bootstrapAlways, run: pathOperation("prepare_tools", []string{"names"}, []string{"out", "target"}, prepareTools)},
	"sync_venv":       {packages: []string{"venv"}, bootstrap: bootstrapPolicy, run: runSyncVenv},
	"venv_is_current": {packages: []string{"venv"}, bootstrap: bootstrapNever, run: runVenvIsCurrent},
	"build_environment": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: pathOperation("build_environment", nil, []string{"source", "out"}, buildEnvironment)},
	"lock_project": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: voidOperation("lock_project", []string{"source"}, nil, lockProject)},
	"ensure_environment": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: pathOperation("ensure_environment", []string{"name", "requirements"}, nil, ensureEnvironment)},
	"ensure_project_environment": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: pathOperation("ensure_project_environment", []string{"name", "project"}, nil, ensureProjectEnvironment)},
	"ensure_python_tool": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: pathOperation("ensure_python_tool", []string{"name", "requirements", "executable"}, nil, ensurePythonTool)},
	"check_project_lock": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: voidOperation("check_project_lock", []string{"source"}, nil, checkProjectLock)},
	"export_requirements": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: voidOperation("export_requirements", []string{"source", "out"}, nil, func(ctx context.Context, opts ExportRequirementsOptions) error {
			_, err := exportRequirements(ctx, opts)
			return err
		})},
	"build_requirements_environment": {packages: []string{"uv"}, bootstrap: bootstrapPolicy,
		run: pathOperation("build_requirements_environment", []string{"requirements"}, []string{"out"}, buildRequirementsEnvironment)},
	"prune_cache": {packages: []string{"uv"}, bootstrap: bootstrapAlways,
		run: voidOperation("prune_cache", []string{"cache"}, nil, pruneCache)},
}

// Routed operation of a name, the KeyError Python raises for one the worker does not route
func lookupOperation(name string) (*workerOperation, error) {
	spec, ok := workerOperations[name]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyRepr(name)}
	}
	return spec, nil
}
