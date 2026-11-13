package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Nix module settings JSON with the package config version added when absent, as json.dump writes it
func NixRenderSettings(settingsJSON string) (string, error) {
	v, err := pyjson.Loads(settingsJSON)
	if err != nil {
		return "", err
	}
	obj, ok := pyjson.AsObject(v)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'setdefault'", pyrepr.TypeName(v))
	}
	if _, has := obj.Get("_config_version"); !has {
		obj.Set("_config_version", Defaults()["_config_version"])
	}
	return pyjson.Dumps(obj)
}

// Deep merges the Nix settings JSON into config.yaml, Nix keys replacing those on disk and every other key kept
func NixMergeConfig(nixJSONPath, configPath string) error {
	raw, err := os.ReadFile(nixJSONPath)
	if err != nil {
		return err
	}
	nixValue, err := pyjson.LoadsPairsHook(string(raw), orderedJSONMapping)
	if err != nil {
		return err
	}
	var existing any = map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil {
		loaded, err := SafeLoad(data)
		if err != nil {
			return err
		}
		if pyrepr.Truthy(loaded) {
			existing = loaded
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	base, err := pyDictOf(existing)
	if err != nil {
		return err
	}
	nix, ok := nixValue.(map[string]any)
	if !ok {
		return fmt.Errorf("AttributeError: '%s' object has no attribute 'items'", pyrepr.TypeName(nixValue))
	}
	merged := nixDeepMerge(base, nix)
	opts := SafeDumpOptions{AllowUnicode: true, Width: 80, Indent: &RuamelDefaultIndent, Version: "1.1"}
	text, err := SafeDumpWith(merged, opts)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, text, 0o666)
}

// JSON object built as a string keyed map that keeps first occurrence order and the last value of a repeated key
func orderedJSONMapping(keys []string, vals []any) (any, error) {
	m := make(map[string]any, len(keys))
	order := make([]string, 0, len(keys))
	for i, k := range keys {
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = vals[i]
	}
	SetMappingKeys(m, order)
	return m, nil
}

// Override merged into a copy of base, nested mappings merged and every other value replaced in place or appended
func nixDeepMerge(base, override map[string]any) map[string]any {
	result, _ := pyDictOf(base)
	for _, k := range MappingKeys(override) {
		v := override[k]
		cur, inResult := result[k]
		curMap, curIsMap := cur.(map[string]any)
		vMap, vIsMap := v.(map[string]any)
		if inResult && curIsMap && vIsMap {
			result[k] = nixDeepMerge(curMap, vMap)
			continue
		}
		if !inResult {
			AppendMappingKey(result, k)
		}
		result[k] = v
	}
	return result
}
