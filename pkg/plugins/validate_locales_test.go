package plugins

import (
	"path/filepath"
	"reflect"
	"strings"
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

func TestLanguagePackValidation(t *testing.T) {
	probe := vtProbe(t)
	pl := []any{"pl"}
	t.Run("manifest only pack passes and unknown key warns by name", func(t *testing.T) {
		r := vtNativeChecks(t, vtPack(t, t.TempDir(), map[string]any{"approval": map[string]any{"denied": "      ✗ Odrzucono", "no_such_key": "x"}}, nil, pl, ""), probe)
		if !r.OK() {
			t.Fatal(r.Failures())
		}
		if c := vtCheck(t, r, "loadable"); !c.OK || !strings.Contains(c.Detail, "language pack") {
			t.Fatalf("%+v", c)
		}
		if c := vtCheck(t, r, "locale pl"); !c.OK || !strings.Contains(c.Detail, "1 match") {
			t.Fatalf("%+v", c)
		}
		warned := false
		for _, w := range r.Warnings {
			warned = warned || (strings.Contains(w, "approval.no_such_key") && strings.Contains(w, "pl.yaml"))
		}
		if !warned {
			t.Fatal(r.Warnings)
		}
	})
	t.Run("non text leaf is an error", func(t *testing.T) {
		r := vtNativeChecks(t, vtPack(t, t.TempDir(), map[string]any{"approval": map[string]any{"denied": 42, "cancelled": []any{"a", "b"}}}, nil, pl, ""), probe)
		c := vtCheck(t, r, "locale pl")
		if r.OK() || c.OK || !strings.Contains(c.Detail, "approval.denied") || !strings.Contains(c.Detail, "approval.cancelled") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("declared language without file fails", func(t *testing.T) {
		r := vtNativeChecks(t, vtPack(t, t.TempDir(), nil, nil, pl, ""), probe)
		if r.OK() || !strings.Contains(strings.Join(r.Failures(), "\n"), "locales/pl.yaml is missing") {
			t.Fatal(r.Failures())
		}
	})
	t.Run("tui surface is checked against the export when present", func(t *testing.T) {
		tmp := t.TempDir()
		fake := filepath.Join(tmp, "bundled")
		vtWrite(t, filepath.Join(fake, "en.yaml"), "approval:\n  denied: Denied\n")
		t.Setenv("ICEBATH_BUNDLED_LOCALES", fake)
		plugin := vtPack(t, tmp, map[string]any{"approval": map[string]any{"denied": "Odrzucono"}}, map[string]any{"status": map[string]any{"ready": "Gotowy", "bogus": "x"}}, pl, "")
		skipped := vtNativeChecks(t, plugin, probe)
		if c := vtCheck(t, skipped, "locale pl.tui"); !skipped.OK() || !c.OK || !strings.Contains(c.Detail, "skipped") {
			t.Fatalf("%+v %v", c, skipped.Failures())
		}
		vtWrite(t, filepath.Join(fake, "_keys.tui.json"), `{"keys": ["status.ready"]}`)
		checked := vtNativeChecks(t, plugin, probe)
		warned := false
		for _, w := range checked.Warnings {
			warned = warned || (strings.Contains(w, "status.bogus") && strings.Contains(w, "pl.tui.yaml"))
		}
		if !checked.OK() || !warned || !reflect.DeepEqual(ReferenceKeys("tui"), map[string]bool{"status.ready": true}) {
			t.Fatalf("%v %v", checked.Failures(), checked.Warnings)
		}
	})
	t.Run("pack with register function still probes", func(t *testing.T) {
		r := vtNativeChecks(t, vtPack(t, t.TempDir(), map[string]any{"approval": map[string]any{"denied": "Odrzucono"}}, nil, pl,
			"def register(ctx):\n    ctx.register_locale('pl', {'approval': {'cancelled': 'Anulowano'}})\n"), probe)
		if !r.OK() || !vtCheck(t, r, "capability probe").OK {
			t.Fatal(r.Failures())
		}
	})
}
