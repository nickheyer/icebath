package tuiserver

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

const catalogSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Catalog entry the way the curated document spells one
func catalogEntry(name, repo, subdir, tier string) *plugins.PluginCatalogEntry {
	if repo == "" {
		repo = "https://github.com/fx/" + name
	}
	return plugins.EntryFromMapping(map[string]any{"name": name, "repo": repo, "sha": catalogSHA, "description": "d.",
		"maintainer": "fx", "subdir": subdir, "tier": tier}, name)
}

// Lookup result of a name, failing the case on an error response
func catalogLookup(t *testing.T, name string) map[string]any {
	t.Helper()
	return resultOf(t, callMethod(t, "plugins.catalog.lookup", map[string]any{"name": name}))
}

// Installs the weather, aihubmix and killed pulled entries for the case
func fakeCatalog(t *testing.T) {
	entries := []*plugins.PluginCatalogEntry{catalogEntry("weather", "", "", "community"),
		catalogEntry("aihubmix", "https://github.com/AIhubmix/mono", "aihubmix", "official"), catalogEntry("pulled", "", "", "community")}
	savedLoad, savedRemoved := catalogLoadLive, catalogRemovedEntries
	catalogLoadLive = func(context.Context) []*plugins.PluginCatalogEntry { return entries }
	catalogRemovedEntries = func(context.Context) []plugins.RemovedEntry { return []plugins.RemovedEntry{{Name: "pulled"}} }
	t.Cleanup(func() { catalogLoadLive, catalogRemovedEntries = savedLoad, savedRemoved })
}

func TestAListedNameReturnsWhatTheInstallDialogNeeds(t *testing.T) {
	fakeCatalog(t)
	want := map[string]any{"status": "found", "entry": map[string]any{"name": "aihubmix", "repo": "https://github.com/AIhubmix/mono",
		"sha": catalogSHA, "subdir": "aihubmix", "tier": "official"}}
	if got := catalogLookup(t, "aihubmix"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := catalogLookup(t, " weather ")["entry"].(map[string]any)["subdir"]; got != "" {
		t.Fatalf("subdir %v", got)
	}
	if got := catalogLookup(t, "weather")["entry"].(map[string]any)["tier"]; got != "community" {
		t.Fatalf("tier %v", got)
	}
}

func TestAnUnlistedOrRemovedNameIsUnknownNeverAGuess(t *testing.T) {
	fakeCatalog(t)
	for _, name := range []string{"weather-evil", "pulled"} {
		if got := catalogLookup(t, name); !reflect.DeepEqual(got, map[string]any{"status": "unknown"}) {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

func TestAnythingButACatalogNameIsInvalid(t *testing.T) {
	fakeCatalog(t)
	for _, name := range []string{"", "../etc", "https://github.com/x/y", "Weather", strings.Repeat("a", 65)} {
		if got := catalogLookup(t, name); !reflect.DeepEqual(got, map[string]any{"status": "invalid_name"}) {
			t.Fatalf("%q: %v", name, got)
		}
	}
}

func TestAnUnreadableCatalogIsUnavailable(t *testing.T) {
	saved := catalogLoadLive
	catalogLoadLive = func(context.Context) []*plugins.PluginCatalogEntry { return nil }
	t.Cleanup(func() { catalogLoadLive = saved })
	if got := catalogLookup(t, "weather"); !reflect.DeepEqual(got, map[string]any{"status": "unavailable"}) {
		t.Fatalf("got %v", got)
	}
}

func TestTheLookupReadsTheBackendCatalogNotTheDocsRedirect(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	first := plugins.LoadCatalog("")[0]
	if got := catalogLookup(t, first.Name)["entry"].(map[string]any)["sha"]; got != first.Sha {
		t.Fatalf("sha %v want %v", got, first.Sha)
	}
}
