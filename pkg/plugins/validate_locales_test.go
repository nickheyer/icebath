package plugins

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Polish language pack fixture with optional core and tui files and an optional __init__.py
func vtPack(t *testing.T, root string, core, tui map[string]any, declared []any, init string) string {
	t.Helper()
	plugin := filepath.Join(root, "icebath-lang-pl")
	manifest, _ := yaml.Marshal(map[string]any{"name": "icebath-lang-pl", "version": "1.0.0", "description": "Polish pack", "provides_locales": declared})
	vtWrite(t, filepath.Join(plugin, "plugin.yaml"), string(manifest))
	vtWrite(t, filepath.Join(plugin, "locales", ".keep"), "")
	if core != nil {
		raw, _ := yaml.Marshal(core)
		vtWrite(t, filepath.Join(plugin, "locales", "pl.yaml"), string(raw))
	}
	if tui != nil {
		raw, _ := yaml.Marshal(tui)
		vtWrite(t, filepath.Join(plugin, "locales", "pl.tui.yaml"), string(raw))
	}
	if init != "" {
		vtWrite(t, filepath.Join(plugin, "__init__.py"), init)
	}
	return plugin
}

func vtCheck(t *testing.T, r *ValidationReport, name string) ValidationCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return ValidationCheck{}
}
