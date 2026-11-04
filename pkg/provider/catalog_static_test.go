package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Drops the canonical row tables so the next read rebuilds them from the profiles
func resetCatalogTables() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogOnce = sync.Once{}
	catalogBuilt = false
	pluginPlacements = map[string]PluginPickerPlacement{}
	pluginCatalogAliases = map[string]string{}
}

var testCodexModels = []string{"gpt-5.3-codex", "gpt-5.2-codex", "gpt-5.1-codex"}

// Fresh curated table and models.dev state against an empty temp home
func resetCuratedCatalog(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, tmp)
	ResetModelsDevCache()
	resetCatalogTables()
	prevCodex := codexCuratedModelsFn
	codexCuratedModelsFn = func() []string { return cloneStrings(testCodexModels) }
	t.Cleanup(func() { codexCuratedModelsFn = prevCodex })
	t.Cleanup(resetCatalogTables)
	providerModelsMu.Lock()
	providerModels = nil
	providerModelsOnce = sync.Once{}
	xaiReady = false
	providerModelsMu.Unlock()
	t.Cleanup(func() {
		ResetModelsDevCache()
		providerModelsMu.Lock()
		providerModels = nil
		providerModelsOnce = sync.Once{}
		xaiReady = false
		providerModelsMu.Unlock()
	})
	return tmp
}

func TestOpenRouterSnapshotDescriptions(t *testing.T) {
	byID := map[string]string{}
	for _, row := range OpenRouterModels {
		byID[row.ID] = row.Description
	}
	if byID["z-ai/glm-5.3"] != "default" || byID["moonshotai/kimi-k3"] != "recommended" {
		t.Fatalf("curated descriptions lost: %v", byID)
	}
	if byID["minimax/minimax-m3:free"] != "free" || byID["stealth/union-alpha"] != "free, stealth model" {
		t.Fatalf("free descriptions wrong: %q %q", byID["minimax/minimax-m3:free"], byID["stealth/union-alpha"])
	}
	if byID["openai/gpt-5.5"] != "" {
		t.Fatalf("undescribed model got %q", byID["openai/gpt-5.5"])
	}
	if VercelAIGatewayModels[0].Description != "recommended" || len(CatalogModelIDs(VercelAIGatewayModels)) != 15 {
		t.Fatalf("vercel snapshot wrong: %+v", VercelAIGatewayModels[0])
	}
}

func TestNousRowDropsOpenRouterOnlyAndFreeEntries(t *testing.T) {
	resetCuratedCatalog(t)
	nous := staticCatalogModels("nous")
	if len(nous) == 0 {
		t.Fatal("empty nous row")
	}
	for _, mid := range nous {
		if openRouterOnly[mid] || strings.HasSuffix(mid, ":free") {
			t.Fatalf("nous row carries %s", mid)
		}
	}
	if !containsString(nous, "meta/muse-spark-1.2") || containsString(nous, "meta/muse-spark-1.2-contributor") {
		t.Fatalf("nous row muse rows wrong: %v", nous)
	}
	if got := staticCatalogModels("ai-gateway"); !reflect.DeepEqual(got, CatalogModelIDs(VercelAIGatewayModels)) {
		t.Fatalf("ai-gateway row: %v", got)
	}
	if got := staticCatalogModels("azure-foundry"); len(got) != 0 || !HasCuratedCatalog("azure-foundry") {
		t.Fatalf("azure-foundry row: %v", got)
	}
	if staticCatalogModels("no-such-provider") != nil || HasCuratedCatalog("no-such-provider") {
		t.Fatal("unknown provider has a row")
	}
	if got := staticCatalogModels("openai-codex"); !reflect.DeepEqual(got, testCodexModels) {
		t.Fatalf("codex row: %v", got)
	}
}

func TestStaticCatalogRowsAreCopies(t *testing.T) {
	resetCuratedCatalog(t)
	row := staticCatalogModels("zai")
	row[0] = "mutated"
	if staticCatalogModels("zai")[0] == "mutated" {
		t.Fatal("caller mutation leaked into the table")
	}
	providers := CuratedCatalogProviders()
	if !containsString(providers, "bedrock") || providers[0] > providers[len(providers)-1] {
		t.Fatalf("providers unsorted or incomplete: %v", providers)
	}
}

func TestXAIFinalizeCatalog(t *testing.T) {
	got := xaiFinalizeCatalog([]string{"grok-a", "grok-z"})
	want := []string{"grok-4.6", "grok-a", "grok-z", "grok-4.5", "grok-composer-2.5-fast"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finalize: %v", got)
	}
	pinned := xaiFinalizeCatalog([]string{"grok-4.6", "grok-b"})
	if !reflect.DeepEqual(pinned, []string{"grok-4.6", "grok-composer-2.5-fast", "grok-4.5", "grok-b"}) {
		t.Fatalf("pinned finalize: %v", pinned)
	}
}

func TestXAIFloorReusesTheProcessModelsDevCopyWithoutNetwork(t *testing.T) {
	tmp := resetCuratedCatalog(t)
	registry := map[string]any{"xai": map[string]any{"id": "xai", "models": map[string]any{"grok-z": map[string]any{}, "grok-a": map[string]any{}}}}
	raw, _ := json.Marshal(registry)
	if err := os.MkdirAll(home.CacheDir(tmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.CacheDir(tmp), "models_dev_cache.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	downloads := 0
	prev := downloadModelsDev
	downloadModelsDev = func(ctx context.Context, url string, headers map[string]string) (int, any, string, error) {
		downloads++
		t.Fatal("network")
		return 0, nil, "", nil
	}
	t.Cleanup(func() { downloadModelsDev = prev })
	ids := staticCatalogModels("xai")
	if !containsString(ids, "grok-a") || !containsString(ids, "grok-z") || ids[0] != "grok-4.6" {
		t.Fatalf("xai row: %v", ids)
	}
	if !reflect.DeepEqual(staticCatalogModels("xai-oauth"), ids) {
		t.Fatal("xai-oauth row differs from xai")
	}
	ctx := home.WithOverride(context.Background(), tmp)
	offline := ModelsDevOffline(ctx)
	if _, ok := offline["xai"]; !ok {
		t.Fatal("process copy not shared")
	}
	if got := FetchModelsDev(ctx, false); !reflect.DeepEqual(got, offline) || downloads != 0 {
		t.Fatal("fresh copy should be served from memory")
	}
	rows := CuratedCatalogRows()
	if !reflect.DeepEqual(rows["xai"], ids) {
		t.Fatalf("copy xai row: %v", rows["xai"])
	}
}

func TestXAIRowsFallBackToStaticListWithoutCache(t *testing.T) {
	resetCuratedCatalog(t)
	ids := staticCatalogModels("xai-oauth")
	if ids[0] != "grok-4.6" || !containsString(ids, "grok-build-0.1") || !containsString(ids, "grok-composer-2.5-fast") {
		t.Fatalf("static fallback: %v", ids)
	}
	for _, retired := range []string{"grok-4", "grok-4-fast", "grok-code-fast-1"} {
		if containsString(ids, retired) {
			t.Fatalf("retired %s listed", retired)
		}
	}
}

func TestPluginProfilesAreAdmittedBySlugNotAuthType(t *testing.T) {
	resetCuratedCatalog(t)
	if !PluginProviderEntersPicker("acme-plugin") {
		t.Fatal("new slug refused")
	}
	if PluginProviderEntersPicker("bedrock") {
		t.Fatal("built in slug admitted twice")
	}
	count := 0
	for _, p := range CanonicalProviders() {
		if p.Slug == "bedrock" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("bedrock rows: %d", count)
	}
	for _, slug := range []string{"deepinfra", "upstage", "solstice", "meta-ai", "router", "custom"} {
		if !IsCanonicalProvider(slug) {
			t.Fatalf("plugin profile %s missing from the row table", slug)
		}
	}
	if SyncPluginProviderCatalog() != 0 {
		t.Fatal("second sync admitted rows again")
	}
	if got := CatalogProviderLabel("deepinfra"); got != "DeepInfra" {
		t.Fatalf("label: %q", got)
	}
	if got := CatalogProviderLabel("nous"); got != "Nous Portal" {
		t.Fatalf("builtin label: %q", got)
	}
	if !IsAggregatorProvider("openrouter") || !IsAggregatorProvider("kilocode") || IsAggregatorProvider("zai") {
		t.Fatalf("aggregator set: %v", AggregatorProviders())
	}
}

func TestHiddenProfilesStayOutOfTheListedRowsUntilOptedIn(t *testing.T) {
	resetCuratedCatalog(t)
	listed := map[string]bool{}
	for _, p := range listedCanonicalProviders() {
		listed[p.Slug] = true
	}
	if listed["solstice"] {
		t.Fatal("hidden profile listed without a credential pool row")
	}
	if !listed["nous"] || !listed["deepinfra"] {
		t.Fatal("visible rows missing")
	}
	hidden := unlistedProviderNames()
	if !hidden["solstice"] || hidden["nous"] {
		t.Fatalf("unlisted set: %v", hidden)
	}
}

func TestRegisterPluginPickerPlacement(t *testing.T) {
	resetCuratedCatalog(t)
	RegisterPluginPickerPlacement("upstage", PluginPickerPlacement{After: "nous", Group: ProviderGroup{ID: "solar", Label: "Solar", Description: "Upstage Solar"}, CatalogAliases: true})
	rows := CanonicalProviders()
	RegisterPluginPickerPlacement("deepinfra", PluginPickerPlacement{After: "nous"})
	if CanonicalProviders()[1].Slug != "upstage" {
		t.Fatal("a placement registered after admission must not rewrite the admitted row")
	}
	idx := map[string]int{}
	for i, p := range rows {
		idx[p.Slug] = i
	}
	if idx["upstage"] != idx["nous"]+1 {
		t.Fatalf("upstage not anchored after nous: %d vs %d", idx["upstage"], idx["nous"])
	}
	if ProviderGroupForSlug("upstage") != "solar" {
		t.Fatal("group not registered")
	}
	if target, ok := CatalogProviderAlias("solar"); !ok || target != "upstage" {
		t.Fatalf("catalog alias: %q %v", target, ok)
	}
}

func TestReverseIndexMatchesGroups(t *testing.T) {
	resetCuratedCatalog(t)
	for _, g := range ProviderGroups() {
		for _, m := range g.Members {
			if ProviderGroupForSlug(m) != g.ID {
				t.Fatalf("%s not in %s", m, g.ID)
			}
		}
	}
	if ProviderGroupForSlug("openrouter") != "" || ProviderGroupForSlug("") != "" {
		t.Fatal("ungrouped slug has a group")
	}
}

func TestMultiMemberGroupFoldsToOneRow(t *testing.T) {
	resetCuratedCatalog(t)
	rows := GroupProviders([]string{"minimax", "minimax-oauth", "minimax-cn"})
	if len(rows) != 1 || rows[0].Kind != "group" || rows[0].GroupID != "minimax" {
		t.Fatalf("rows: %+v", rows)
	}
	if !reflect.DeepEqual(rows[0].Members, []string{"minimax", "minimax-oauth", "minimax-cn"}) {
		t.Fatalf("members: %v", rows[0].Members)
	}
	var minimax ProviderGroup
	for _, g := range ProviderGroups() {
		if g.ID == "minimax" {
			minimax = g
		}
	}
	if rows[0].Description != minimax.Description || rows[0].Description == "" {
		t.Fatalf("description: %q", rows[0].Description)
	}
	single := GroupProviders([]string{"openrouter", "xai", "nous", "XAI", ""})
	if len(single) != 3 || single[0].Kind != "single" || single[1].Kind != "single" || single[1].Slug != "xai" {
		t.Fatalf("single member group should stay a single row: %+v", single)
	}
	folded := GroupProviders([]string{"kimi-coding-cn", "openrouter", "kimi-coding"})
	if len(folded) != 2 || folded[0].Kind != "group" || !reflect.DeepEqual(folded[0].Members, []string{"kimi-coding", "kimi-coding-cn"}) {
		t.Fatalf("group should sit at the first member and keep declaration order: %+v", folded)
	}
}

func TestCatalogProviderAliasesAndFlags(t *testing.T) {
	resetCuratedCatalog(t)
	for alias, want := range map[string]string{"glm": "zai", "ollama": "custom", "local": "custom", "qwen": "alibaba", "chatgpt": "openai-codex", "tencent": "tencent-tokenhub"} {
		if got, ok := CatalogProviderAlias(alias); !ok || got != want {
			t.Fatalf("%s -> %q %v", alias, got, ok)
		}
	}
	if _, ok := CatalogProviderAlias("llamacpp"); ok {
		t.Fatal("llamacpp must stay unmapped")
	}
	if PreferredSilentDefaultModel != "z-ai/glm-5.3" || !UsesSilentCatalogDefault("nous") || UsesSilentCatalogDefault("zai") {
		t.Fatal("silent default tables wrong")
	}
	if !reflect.DeepEqual(ProviderRetiredAliases("deepseek"), []string{"deepseek-chat", "deepseek-reasoner"}) || ProviderRetiredAliases("zai") != nil {
		t.Fatal("retired aliases wrong")
	}
	if !IsLiveFirstPickerProvider("opencode-zen") || IsLiveFirstPickerProvider("nous") || !IsModelsDevPreferred("xai-oauth") || IsModelsDevPreferred("openrouter") || IsBorrowedModelProvider("nous") {
		t.Fatal("picker policy sets wrong")
	}
}
