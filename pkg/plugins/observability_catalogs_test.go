package plugins

import (
	"io/fs"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestInTreeCatalogFeedsTheSharedMetricsCatalogs(t *testing.T) {
	observability.ResetCatalogCaches()
	stems, err := pluginCatalogStems()
	files, _ := fs.Glob(inTreeCatalog, "catalog/*.yaml")
	if err != nil || len(stems) != len(files) || len(files) == 0 {
		t.Fatalf("stems %d files %d %v", len(stems), len(files), err)
	}
	for _, name := range []string{"honcho", "ando-platform"} {
		if !observability.ExtensionNames.Values().Has(name) {
			t.Fatalf("plugin catalog name %s missing from the extension vocabulary", name)
		}
	}
	if got := observability.MemoryProviderName("honcho"); got != "honcho" {
		t.Fatalf("catalogued memory provider reads %s", got)
	}
	if got := observability.MemoryProviderName("acme-private-memory"); got != "plugin" {
		t.Fatalf("private memory provider reads %s", got)
	}
	platforms := observability.GatewayPlatforms.Values()
	if !platforms.Has("ando-platform") {
		t.Fatal("platform catalog entry missing from the gateway platform vocabulary")
	}
	for _, e := range LoadCatalog("") {
		if e.Category != "platform" && platforms.Has(e.Name) && e.Name != "api_server" {
			names, _ := catalogPlatformNames()
			for _, n := range names {
				if n == e.Name {
					t.Fatalf("%s is category %s yet reads as a catalog platform", e.Name, e.Category)
				}
			}
		}
	}
}
