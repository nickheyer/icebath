package skills

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestInstalledEntryExportMatchesDirectAndClawHubAliasKeys(t *testing.T) {
	direct := pyjson.NewObject()
	direct.Set("name", "direct")
	aliased := pyjson.NewObject()
	aliased.Set("name", "aliased")
	alias, ok := clawhubCatalogIdentifier("@alice/weather")
	if !ok {
		t.Fatal("expected a clawhub catalog identifier for @alice/weather")
	}
	installed := map[string]*pyjson.Object{"github/x/y": direct, alias: aliased}
	if got := InstalledEntry(installed, "github/x/y", "github"); got != direct {
		t.Fatalf("direct key: got %v", got)
	}
	if got := InstalledEntry(installed, "@alice/weather", "clawhub"); got != aliased {
		t.Fatalf("clawhub alias: got %v", got)
	}
	if got := InstalledEntry(installed, "@alice/weather", "github"); got != nil {
		t.Fatalf("non clawhub miss must be nil, got %v", got)
	}
	if got := InstalledEntry(installed, "nobody/nothing", "clawhub"); got != nil {
		t.Fatalf("clawhub miss must be nil, got %v", got)
	}
}

func TestResolvedSourcesUsesTheSourcesSeamElseTheHubRouter(t *testing.T) {
	hub, _ := newTestHub(t)
	x := NewHubCLI(hub.S)
	x.Hub = hub
	if got := x.ResolvedSources(); len(got) == 0 {
		t.Fatal("hub router returned no sources")
	}
	x.Sources = func() []SkillSource { return nil }
	if got := x.ResolvedSources(); got != nil {
		t.Fatalf("seam result expected, got %v", got)
	}
}
