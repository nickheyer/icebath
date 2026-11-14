package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Drops pack registrations and the language cache before and after the case
func cleanLayers(t *testing.T) {
	i18n.ResetRegistryForTests()
	i18n.ResetLanguageCache()
	t.Cleanup(func() {
		i18n.ResetRegistryForTests()
		i18n.ResetLanguageCache()
	})
}

// Response of the registered handler of a method
func callMethod(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	fn := lookupMethod(method)
	if fn == nil {
		t.Fatalf("method %s not registered", method)
	}
	return fn(context.Background(), 3, params)
}

// Result member of a response, failing the case on an error response
func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, isDict := resp["result"].(map[string]any)
	if !isDict {
		t.Fatalf("no result in %v", resp)
	}
	return result
}

// Writes text to a file, creating its parents
func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Disables plugin discovery for the case so only the registrations it makes are visible
func swapDiscovery(t *testing.T, fn func(context.Context, bool)) {
	saved := discoverPlugins
	discoverPlugins = fn
	t.Cleanup(func() { discoverPlugins = saved })
}

// Language rows of the languages answer
func languageRows(t *testing.T) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, row := range resultOf(t, callMethod(t, "i18n.languages", map[string]any{}))["languages"].([]any) {
		rows = append(rows, row.(map[string]any))
	}
	return rows
}

func TestLanguagesAndCatalogServePackLayersPerSurface(t *testing.T) {
	cleanLayers(t)
	h := filepath.Join(t.TempDir(), "home")
	writeTestFile(t, filepath.Join(h, "locales", "de.yaml"), "approval:\n  denied: Overlay-DE\n")
	t.Setenv("ICEBATH_HOME", h)
	i18n.ResetLanguageCache()
	swapDiscovery(t, func(context.Context, bool) {})
	pc := plugins.NewPluginContext(&plugins.Manifest{Name: "hermes-lang-pl", Key: "hermes-lang-pl", Source: "user", Path: h}, plugins.NewManager(h))
	for _, reg := range []struct {
		messages map[string]any
		surface  string
	}{
		{map[string]any{"approval": map[string]any{"denied": "Odrzucono"}}, ""},
		{map[string]any{"status": map[string]any{"ready": "Gotowy"}}, "tui"},
		{map[string]any{"settings": map[string]any{"title": "Ustawienia"}}, "desktop"},
	} {
		if _, err := pc.RegisterLocale("pl", reg.messages, "Polski", false, reg.surface); err != nil {
			t.Fatal(err)
		}
	}

	rows := languageRows(t)
	var ids []string
	for _, row := range rows {
		ids = append(ids, row["id"].(string))
	}
	sorted := slices.Clone(ids[1:])
	slices.Sort(sorted)
	if ids[0] != "en" || !slices.Equal(ids[1:], sorted) {
		t.Fatalf("ids %v", ids)
	}
	wantPL := map[string]any{"id": "pl", "endonym": "Polski", "rtl": false, "source": "plugin:hermes-lang-pl", "surfaces": []any{"core", "tui", "desktop"}}
	wantAR := map[string]any{"id": "ar", "endonym": "العربية", "rtl": true, "source": "bundled", "surfaces": []any{"core", "tui"}}
	for _, want := range []map[string]any{wantPL, wantAR} {
		if !slices.ContainsFunc(rows, func(row map[string]any) bool { return reflect.DeepEqual(row, want) }) {
			t.Fatalf("missing %v in %v", want, rows)
		}
	}

	core := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "pl"}))
	if !reflect.DeepEqual(core, map[string]any{"lang": "pl", "surface": "core", "messages": map[string]any{"approval.denied": "Odrzucono"}}) {
		t.Fatalf("core %v", core)
	}
	tui := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "PL", "surface": "tui"}))
	if !reflect.DeepEqual(tui, map[string]any{"lang": "pl", "surface": "tui", "messages": map[string]any{"status.ready": "Gotowy"}}) {
		t.Fatalf("tui %v", tui)
	}
	desktop := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "pl", "surface": "desktop"}))
	if !reflect.DeepEqual(desktop["messages"], map[string]any{"settings.title": "Ustawienia"}) {
		t.Fatalf("desktop %v", desktop)
	}
	de := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "de"}))
	if !reflect.DeepEqual(de["messages"], map[string]any{"approval.denied": "Overlay-DE"}) {
		t.Fatalf("de %v", de)
	}
	unknown := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "xx-nope"}))
	if !reflect.DeepEqual(unknown, map[string]any{"lang": "en", "surface": "core", "messages": map[string]any{}}) {
		t.Fatalf("unknown %v", unknown)
	}
	bad := callMethod(t, "i18n.catalog", map[string]any{"lang": "pl", "surface": "web"})
	if code := bad["error"].(map[string]any)["code"]; code != 4000 && code != 4002 {
		t.Fatalf("bad %v", bad)
	}
}

func TestCatalogAnswersForTheRequestedProfileHome(t *testing.T) {
	cleanLayers(t)
	root := t.TempDir()
	launch := filepath.Join(root, "launch")
	named := filepath.Join(launch, "profiles", "named")
	if err := os.MkdirAll(filepath.Join(launch, "locales"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(named, "locales", "de.yaml"), "approval:\n  denied: Nur named\n")
	t.Setenv("ICEBATH_HOME", launch)
	swapDiscovery(t, func(context.Context, bool) {})
	i18n.ResetLanguageCache()

	got := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "de", "profile": "named"}))["messages"]
	if !reflect.DeepEqual(got, map[string]any{"approval.denied": "Nur named"}) {
		t.Fatalf("named %v", got)
	}
	got = resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "de"}))["messages"]
	if !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("launch %v", got)
	}
}

func TestFailedPluginDiscoveryStillAnswersWithBundledLanguages(t *testing.T) {
	cleanLayers(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapDiscovery(t, func(context.Context, bool) { panic("plugin loader broke") })
	i18n.ResetLanguageCache()

	want := map[string]any{"id": "ko", "endonym": "한국어", "rtl": false, "source": "bundled", "surfaces": []any{"core", "tui"}}
	if rows := languageRows(t); !slices.ContainsFunc(rows, func(row map[string]any) bool { return reflect.DeepEqual(row, want) }) {
		t.Fatalf("missing %v in %v", want, rows)
	}
	got := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "ko", "surface": "desktop"}))["messages"]
	if !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("desktop %v", got)
	}
}

func TestColdGatewayServesAnInstalledPackBeforeAnyAgentExists(t *testing.T) {
	cleanLayers(t)
	root := t.TempDir()
	h := filepath.Join(root, "home")
	plugin := filepath.Join(h, "plugins", "hermes-lang-ko")
	writeTestFile(t, filepath.Join(plugin, "plugin.yaml"), "name: hermes-lang-ko\nversion: 1.0.0\ndescription: Korean pack\nprovides_locales:\n- id: ko\n  endonym: 한국어\n")
	writeTestFile(t, filepath.Join(plugin, "locales", "ko.yaml"), "{}\n")
	writeTestFile(t, filepath.Join(plugin, "locales", "ko.desktop.yaml"), "settings:\n  appearance:\n    modelPricingTitle: 모델 가격\n")
	writeTestFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - hermes-lang-ko\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	i18n.ResetLanguageCache()
	ctx := context.Background()
	manager := plugins.GetPluginManager(ctx)
	if slices.Contains(i18n.PackLanguages(), "ko") {
		t.Fatal("ko pack loaded before discovery")
	}
	t.Cleanup(func() { manager.Unload(nil) })

	var ko map[string]any
	for _, row := range languageRows(t) {
		if row["id"] == "ko" {
			ko = row
		}
	}
	if ko == nil || ko["source"] != "plugin:hermes-lang-ko" || !slices.Contains(ko["surfaces"].([]any), any("desktop")) {
		t.Fatalf("ko row %v", ko)
	}
	desktop := resultOf(t, callMethod(t, "i18n.catalog", map[string]any{"lang": "ko", "surface": "desktop"}))
	if !reflect.DeepEqual(desktop["messages"], map[string]any{"settings.appearance.modelPricingTitle": "모델 가격"}) {
		t.Fatalf("desktop %v", desktop)
	}
}
