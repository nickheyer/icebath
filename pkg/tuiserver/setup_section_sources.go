package tuiserver

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Named values a setup section loads beside its keys, each given the subject its name carries
var setupSectionValues = map[string]func(ctx context.Context, h string, cfg config.Tree, subject string) (any, error){}

// Setup side effects that take the parameters of the walk, each printing to out and reporting success
var setupParamActions = map[string]func(ctx context.Context, h string, params map[string]string, out io.Writer) bool{}

// Values of the asked names keyed as asked, an unknown name reported as false
func loadSetupValues(ctx context.Context, h string, cfg config.Tree, names []string) (map[string]any, bool, error) {
	values := map[string]any{}
	for _, name := range names {
		kind, subject, _ := strings.Cut(name, ":")
		source, known := setupSectionValues[kind]
		if !known {
			return nil, false, nil
		}
		value, err := source(ctx, h, cfg, subject)
		if err != nil {
			return nil, true, err
		}
		values[name] = value
	}
	return values, true, nil
}

// Text parameters of an action request in either map type the transport delivers
func setupParamText(raw any) map[string]string {
	out := map[string]string{}
	switch params := raw.(type) {
	case *pyjson.Object:
		for _, key := range params.Keys {
			out[key], _ = params.Vals[key].(string)
		}
	case map[string]any:
		for key, value := range params {
			out[key], _ = value.(string)
		}
	}
	return out
}

// Action of a name bound to the text parameters of the request, false for an unknown name
func setupActionFor(name string, raw any) (func(ctx context.Context, h string, out io.Writer) bool, bool) {
	action, known := setupParamActions[name]
	if !known {
		return nil, false
	}
	params := setupParamText(raw)
	return func(ctx context.Context, h string, out io.Writer) bool { return action(ctx, h, params, out) }, true
}

// Sets a dotted config key, creating missing levels and refusing a level that is not a mapping
func setDottedKey(cfg map[string]any, key string, value any) error {
	parts := strings.Split(key, ".")
	current := cfg
	for _, part := range parts[:len(parts)-1] {
		raw, present := current[part]
		switch next := raw.(type) {
		case map[string]any:
			current = next
		case config.Tree:
			current = next
		default:
			if present {
				return fmt.Errorf("TypeError: %s holds a %s, not a mapping", part, pyrepr.TypeName(raw))
			}
			level := map[string]any{}
			current[part] = level
			current = level
		}
	}
	current[parts[len(parts)-1]] = value
	return nil
}

// Deletes the dotted key from a config tree, a missing level leaving it as it is
func deleteDottedKey(cfg map[string]any, key string) {
	parts := strings.Split(key, ".")
	current := cfg
	for _, part := range parts[:len(parts)-1] {
		switch next := current[part].(type) {
		case map[string]any:
			current = next
		case config.Tree:
			current = next
		default:
			return
		}
	}
	delete(current, parts[len(parts)-1])
}

// Writes config answers through the whole config save the setup wizard ends with, after a pre-setup backup
func fullSetupSave(ctx context.Context, h string, sets []config.KeyValue, unsets []string) error {
	if err := config.EnsureIcebathHome(ctx, h); err != nil {
		return err
	}
	if _, err := config.BackupConfig(ctx, home.ConfigPath(h), "pre-setup", config.DefaultBackupKeep); err != nil {
		return err
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	for _, kv := range sets {
		if err := setDottedKey(cfg, kv.Key, kv.Value); err != nil {
			return err
		}
	}
	for _, key := range unsets {
		deleteDottedKey(cfg, key)
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}
