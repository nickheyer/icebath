package provider

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestMissingConfigFieldsIgnoresUserNamedPresetsButChecksSchemaOptions(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	current := map[string]any{"moa": map[string]any{"presets": map[string]any{"keep_a": map[string]any{}}}, "terminal": map[string]any{}}
	missing := map[string]bool{}
	for _, field := range config.MissingConfigFields(current) {
		missing[field.Key] = true
	}
	for key := range missing {
		if strings.HasPrefix(key, "moa.presets.") {
			t.Fatalf("user preset compared against the schema %s", key)
		}
	}
	if !missing["terminal.backend"] {
		t.Fatalf("terminal.backend not reported %v", missing)
	}
}
