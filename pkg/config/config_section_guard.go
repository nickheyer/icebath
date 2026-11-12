package config

import (
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Refusal of a config set that the caller prints to stderr before exiting with status one
type ConfigSetRefusal struct{ Message string }

func (e *ConfigSetRefusal) Error() string { return e.Message }

// Key to write after refusing or redirecting a single segment key that would overwrite a config section
func GuardSectionOverwrite(key string, value any, userConfig map[string]any, force bool, out io.Writer) (string, error) {
	existing := AsMap(userConfig[key])
	isMap := AsMap(value) != nil
	isList := !isMap && value != nil && reflect.ValueOf(value).Kind() == reflect.Slice
	kind := "list"
	if isMap {
		kind = "mapping"
	}
	if key == "model" && !force && (isList || (isMap && existing != nil)) {
		return "", &ConfigSetRefusal{Message: "✗ Cannot set 'model' to a " + kind + " — " +
			"the bare 'model' shorthand takes a model id, and a container value has no " +
			"slot there.\n" +
			"  Set the section's keys individually instead:\n" +
			"    icebath config set model.provider <provider>\n" +
			"    icebath config set model.default <model-id>\n" +
			"  Or replace the whole section deliberately:\n" +
			"    icebath config set --force model '{provider: <provider>, default: <model-id>}'"}
	}
	if strings.Contains(key, ".") || existing == nil {
		return key, nil
	}
	if key == "model" {
		if force {
			what := "a scalar"
			if isMap || isList {
				what = "the given " + kind
			}
			fmt.Fprintf(out, "⚠ Replacing entire 'model' section with %s (discarding %d existing sub-key(s))\n", what, len(existing))
			return key, nil
		}
		fmt.Fprintf(out, "✓ Redirecting bare 'model' to 'model.default' (preserving %d existing model sub-key(s))\n", len(existing))
		return "model.default", nil
	}
	if force {
		return key, nil
	}
	sub := MappingKeys(existing)
	lines := []string{fmt.Sprintf("✗ Cannot set '%s' to a scalar — '%s' is a configuration section with %d sub-key(s).", key, key, len(sub))}
	if len(sub) > 0 {
		lines = append(lines, "  Sub-keys: "+strings.Join(sub[:min(8, len(sub))], ", "))
		if len(sub) > 8 {
			lines = append(lines, fmt.Sprintf("  ... and %d more", len(sub)-8))
		}
	}
	lines = append(lines,
		"  Use a dotted path to set a specific leaf key:",
		fmt.Sprintf("    icebath config set %s.<sub-key> <value>", key),
		"  Or use --force to replace the entire section:",
		fmt.Sprintf("    icebath config set --force %s %s", key, PyRepr(value)))
	return "", &ConfigSetRefusal{Message: strings.Join(lines, "\n")}
}
