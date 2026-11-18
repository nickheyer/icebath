//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Routes the package manager's worker operations beside the binary store's ensure
func init() {
	for name, op := range map[string]*workerOperation{
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
	} {
		workerOperations[name] = op
	}
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

// Assigns one decoded JSON value to a field of the options struct
func assignKwarg(op, name string, field reflect.Value, value any) error {
	switch field.Type() {
	case reflect.TypeOf(time.Duration(0)):
		seconds, err := jsonNumber(value)
		if err != nil {
			return kwargTypeError(op, name, "a whole number", value)
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
			return kwargTypeError(op, name, "text", value)
		}
		field.SetString(s)
		return nil
	case reflect.TypeOf(false):
		field.SetBool(pyrepr.Truthy(value))
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
				return kwargTypeError(op, name, "a mapping of text", v)
			}
			out[k] = s
		}
		field.Set(reflect.ValueOf(out))
		return nil
	}
	if field.Kind() == reflect.Int {
		n, err := jsonNumber(value)
		if err != nil {
			return kwargTypeError(op, name, "a whole number", value)
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

// Optional string list keyword, nil for an absent or null value
func (k *kwargs) list(name string) ([]string, error) {
	v, ok := k.take(name)
	if !ok || v == nil {
		return nil, nil
	}
	return pyIterStrings(v)
}

// Plugin input keyword decoded from its wire form, nil when absent or null
func (k *kwargs) plugins() (PluginInput, error) {
	raw, ok := k.take("plugins")
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, isMap := raw.(map[string]any)
	if !isMap {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("the plugins option must be a mapping, got %s", errchain.Kind(raw))}
	}
	return DecodePluginInput(encoded)
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
