package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
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
