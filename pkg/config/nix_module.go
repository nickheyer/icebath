package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Nix module settings JSON with the package config version added when absent, as json.dump writes it
func NixRenderSettings(settingsJSON string) (string, error) {
	v, err := jsonutil.Decode(settingsJSON)
	if err != nil {
		return "", err
	}
	obj, ok := jsonutil.AsObject(v)
	if !ok {
		return "", fmt.Errorf("the Nix settings are %s, expected a mapping", errchain.Kind(v))
	}
	return jsonutil.Encode(obj, jsonutil.Spaced(), jsonutil.ASCII())
}

// Deep merges the Nix settings JSON into config.yaml, Nix keys replacing those on disk and every other key kept
func NixMergeConfig(nixJSONPath, configPath string) error {
	raw, err := os.ReadFile(nixJSONPath)
	if err != nil {
		return err
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		return err
	}
	nixValue := ConfigFromJSON(decoded)
	var existing any = map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil {
		loaded, err := SafeLoad(data)
		if err != nil {
			return err
		}
		if anyutil.IsTrue(loaded) {
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
		return fmt.Errorf("the Nix config %s is %s, expected a mapping", nixJSONPath, errchain.Kind(nixValue))
	}
	merged := nixDeepMerge(base, nix)
	opts := SafeDumpOptions{AllowUnicode: true, Width: 80, Indent: &RuamelDefaultIndent, Version: "1.1"}
	text, err := SafeDumpWith(merged, opts)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, text, 0o666)
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
