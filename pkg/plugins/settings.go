package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
)

var settingsFieldTypes = map[string]string{
	"str": "string", "string": "string",
	"int": "number", "integer": "number", "float": "number", "number": "number",
	"bool": "boolean", "boolean": "boolean",
	"list": "json", "array": "json", "dict": "json", "object": "json",
	"secret": "secret",
}

var envNameCleanRe = regexp.MustCompile(`[^A-Z0-9]+`)

// One key and value of an ordered settings edit
type PluginSettingValue struct {
	Key   string
	Value any
}

// Ordered config schema of one plugin, each entry a declared key and its mapping spec
type schemaEntry struct {
	key  string
	spec map[string]any
}

// Keys and raw values of a JSON object in document order, a repeated key keeping its first position
func decodeRawObject(raw []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("expected a JSON object")
	}
	var keys []string
	values := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, seen := values[key]; !seen {
			keys = append(keys, key)
		}
		values[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("Extra data")
	}
	return keys, values, nil
}

// Keys and decoded values of a JSON object in document order
func DecodeOrderedObject(raw []byte) ([]string, map[string]any, error) {
	keys, rawValues, err := decodeRawObject(raw)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]any, len(rawValues))
	for k, v := range rawValues {
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return nil, nil, err
		}
		values[k] = decoded
	}
	return keys, values, nil
}

// Ordered JSON object reached by walking keys from the root, nil when a step is missing or not an object
func orderedJSONPath(raw []byte, path ...string) ([]string, map[string]any, error) {
	for _, step := range path {
		_, children, err := decodeRawObject(raw)
		if err != nil {
			return nil, nil, err
		}
		child, ok := children[step]
		if !ok || !bytes.HasPrefix(bytes.TrimSpace(child), []byte("{")) {
			return nil, nil, nil
		}
		raw = child
	}
	return DecodeOrderedObject(raw)
}

// Ordered entries of a YAML mapping node whose values are mappings, keys spelled as Python str
func yamlSchemaEntries(node *yaml.Node) []schemaEntry {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var out []schemaEntry
	for i := 0; i+1 < len(node.Content); i += 2 {
		var key, value any
		if node.Content[i].Decode(&key) != nil || node.Content[i+1].Decode(&value) != nil {
			continue
		}
		if m, ok := value.(map[string]any); ok {
			out = append(out, schemaEntry{key: config.PyStr(key), spec: normalizeAnyMaps(m)})
		}
	}
	return out
}

// Config schema of an installed plugin from plugin.yaml or the portable plugin.json vendor extension
func manifestSettingsSchema(pluginDir string) []schemaEntry {
	if pluginDir == "" {
		return nil
	}
	manifest := filepath.Join(pluginDir, "plugin.yaml")
	if st, err := os.Stat(manifest); err == nil && st.Mode().IsRegular() {
		raw, err := os.ReadFile(manifest)
		if err != nil {
			logDebug(fmt.Sprintf("plugin settings: cannot read the manifest in %s: %v", pluginDir, err))
			return nil
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(stripBOM(raw), &doc); err != nil {
			logDebug(fmt.Sprintf("plugin settings: cannot read the manifest in %s: %v", pluginDir, err))
			return nil
		}
		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			return nil
		}
		root := doc.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "config_schema" {
				return yamlSchemaEntries(root.Content[i+1])
			}
		}
		return nil
	}
	portable := filepath.Join(pluginDir, "plugin.json")
	st, err := os.Stat(portable)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(portable)
	if err == nil {
		_, _, err = DecodeOrderedObject(stripBOM(raw))
	}
	if err != nil {
		logDebug(fmt.Sprintf("plugin settings: cannot read the manifest in %s: %v", pluginDir, err))
		return nil
	}
	keys, values, err := orderedJSONPath(stripBOM(raw), "extensions", portableExtension, "config_schema")
	if err != nil || values == nil {
		return nil
	}
	var out []schemaEntry
	for _, k := range keys {
		if m, ok := values[k].(map[string]any); ok {
			out = append(out, schemaEntry{key: k, spec: m})
		}
	}
	return out
}

// Env variable a secret field lives under, the declared env or the upper snaked plugin id and key
func SecretEnvName(pluginID, key string, spec map[string]any) string {
	if declared := strings.TrimSpace(orEmpty(spec["env"])); declared != "" {
		return declared
	}
	upper := cases.Upper(language.Und).String(pluginID + "_" + key)
	return strings.Trim(envNameCleanRe.ReplaceAllString(upper, "_"), "_")
}

// Declared choices of a field, the choices key winning over enum even when null
func fieldChoices(spec map[string]any) any {
	if v, ok := spec["choices"]; ok {
		return v
	}
	return spec["enum"]
}

// Wire field type a schema entry renders as
func settingsFieldType(spec map[string]any) string {
	if b, ok := spec["secret"].(bool); ok && b {
		return "secret"
	}
	name := "str"
	if jsonTruthy(spec["type"]) {
		name = config.PyStr(spec["type"])
	}
	kind, ok := settingsFieldTypes[strings.ToLower(name)]
	if !ok {
		kind = "string"
	}
	if list, ok := fieldChoices(spec).([]any); kind == "string" && ok && len(list) > 0 {
		return "enum"
	}
	return kind
}

// Renderable settings fields of one plugin with current values, secrets carrying only env and presence
func PluginSettingsFields(ctx context.Context, h, pluginID, pluginDir string) []map[string]any {
	schema := manifestSettingsSchema(pluginDir)
	if len(schema) == 0 {
		return []map[string]any{}
	}
	entry := pluginSettingsEntry(loadConfigOrEmpty(h), pluginID)
	current, _ := entry["settings"].(map[string]any)
	fields := []map[string]any{}
	for _, item := range schema {
		if _, err := PluginRelativeSegments(item.key); err != nil {
			continue
		}
		spec := item.spec
		kind := settingsFieldType(spec)
		label := item.key
		if jsonTruthy(spec["label"]) {
			label = config.PyStr(spec["label"])
		} else if jsonTruthy(spec["title"]) {
			label = config.PyStr(spec["title"])
		}
		field := map[string]any{
			"key": item.key, "type": kind, "label": label,
			"description": config.PyStr(orNil(spec["description"])),
			"required":    config.PyTruthy(spec["required"]),
		}
		if kind == "secret" {
			env := SecretEnvName(pluginID, item.key, spec)
			_, found, _ := config.GetEnvValue(ctx, h, env)
			field["env"] = env
			field["has_value"] = found
		} else {
			if kind == "enum" {
				var choices []any
				for _, c := range fieldChoices(spec).([]any) {
					choices = append(choices, config.PyStr(c))
				}
				field["choices"] = choices
			}
			if def, ok := spec["default"]; ok {
				field["default"] = def
			}
			if v, ok := current[item.key]; ok {
				field["value"] = v
			} else {
				field["value"] = spec["default"]
			}
		}
		fields = append(fields, field)
	}
	return fields
}

// Value itself when truthy, otherwise the empty string
func orNil(v any) any {
	if jsonTruthy(v) {
		return v
	}
	return ""
}

// Reports whether a value has the Python type a wire field accepts, bool never counting as a number
func settingsValueTyped(kind string, v any) bool {
	switch kind {
	case "string", "enum":
		_, ok := v.(string)
		return ok
	case "number":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "json":
		switch v.(type) {
		case []any, map[string]any:
			return true
		}
	}
	return false
}

// PluginSettingsValueError is a settings edit the schema refuses, the ValueError a form shows back
type PluginSettingsValueError struct{ Msg string }

func (e *PluginSettingsValueError) Error() string      { return e.Msg }
func (e *PluginSettingsValueError) PyTypeName() string { return "ValueError" }

// Validates every edit against the schema then writes each one, returning the keys written in order
func SavePluginSettings(ctx context.Context, h, pluginID, pluginDir string, values []PluginSettingValue) ([]string, error) {
	schema := map[string]map[string]any{}
	for _, item := range manifestSettingsSchema(pluginDir) {
		schema[item.key] = item.spec
	}
	type planned struct {
		key      string
		segments []string
		value    any
	}
	var plan []planned
	for _, kv := range values {
		spec, ok := schema[kv.Key]
		if !ok {
			return nil, &PluginSettingsValueError{Msg: fmt.Sprintf("%s is not declared in the plugin's config_schema", config.PyRepr(kv.Key))}
		}
		kind := settingsFieldType(spec)
		if kind == "secret" {
			return nil, &PluginSettingsValueError{Msg: fmt.Sprintf("%s is a secret; it is stored in .env, not config.yaml", config.PyRepr(kv.Key))}
		}
		if !settingsValueTyped(kind, kv.Value) {
			return nil, &PluginSettingsValueError{Msg: fmt.Sprintf("%s should be %s (got %s)", config.PyRepr(kv.Key), kind, config.PyTypeName(kv.Value))}
		}
		if kind == "enum" {
			allowed := false
			for _, c := range fieldChoices(spec).([]any) {
				if config.PyStr(c) == kv.Value.(string) {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, &PluginSettingsValueError{Msg: fmt.Sprintf("%s must be one of the declared choices", config.PyRepr(kv.Key))}
			}
		}
		segments, err := PluginRelativeSegments(kv.Key)
		if err != nil {
			return nil, &PluginSettingsValueError{Msg: err.Error()}
		}
		plan = append(plan, planned{kv.Key, segments, kv.Value})
	}
	keys := []string{}
	for _, p := range plan {
		if err := SavePluginSetting(ctx, h, pluginID, p.segments, p.value); err != nil {
			return nil, err
		}
		keys = append(keys, p.key)
	}
	return keys, nil
}
