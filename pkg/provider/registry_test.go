package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Clears the process wide provider registry for one test, discovery counted as done
func resetProviderRegistry(t *testing.T) {
	t.Helper()
	snapshotProviderRegistry(t)
	regMu.Lock()
	regProfiles, regOrder, regAliases, regSources, regListCache = map[string]*Profile{}, nil, map[string]string{}, map[string]string{}, nil
	regMu.Unlock()
	homeLayersMu.Lock()
	homeLayers = map[string]*homeLayer{}
	homeLayersMu.Unlock()
	regGeneration.Add(1)
}

func listedNames(ps []Profile) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// Writes a home plugin dir with an init module so the scan hands it to the plugin host
func writeProviderPluginDir(t *testing.T, h, name string) {
	t.Helper()
	dir := filepath.Join(h, "plugins", "model-providers", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("# plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListProvidersReusesCachedSnapshotUntilRegistrationChanges(t *testing.T) {
	resetProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	RegisterProvider(Profile{Name: "alpha"})
	listed := ListProviders(ctx)
	listed[0].Name = "mutated"
	if got := listedNames(ListProviders(ctx)); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("list = %v", got)
	}
	RegisterProvider(Profile{Name: "beta"})
	if got := listedNames(ListProviders(ctx)); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("list = %v", got)
	}
}

func TestListProvidersDedupesAliasesInCachedSnapshot(t *testing.T) {
	resetProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	RegisterProvider(Profile{Name: "kimi", Aliases: []string{"moonshot", "kimi-k2"}})
	if p, ok := GetProviderProfile(ctx, "moonshot"); !ok || p.Name != "kimi" {
		t.Fatalf("alias lookup = %v %v", p.Name, ok)
	}
	if got := listedNames(ListProviders(ctx)); !reflect.DeepEqual(got, []string{"kimi"}) {
		t.Fatalf("list = %v", got)
	}
}

func TestProviderLookupsReuseTheHomePluginStampWithinItsTTL(t *testing.T) {
	resetProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "icebath"))
	RegisterProvider(Profile{Name: "known"})
	var calls atomic.Int32
	prev := pluginDirStampsFn
	pluginDirStampsFn = func(h string) []int64 { calls.Add(1); return prev(h) }
	t.Cleanup(func() { pluginDirStampsFn = prev })
	homeLayersMu.Lock()
	homeLayers = map[string]*homeLayer{}
	homeLayersMu.Unlock()
	ctx := context.Background()
	if p := getProviderProfile(ctx, "known"); p == nil || p.Name != "known" {
		t.Fatal("known profile missing")
	}
	if got := listedNames(ListProviders(ctx)); !reflect.DeepEqual(got, []string{"known"}) {
		t.Fatalf("list = %v", got)
	}
	getProviderProfile(ctx, "known")
	if calls.Load() != 1 {
		t.Fatalf("stamp checks = %d", calls.Load())
	}
}

func TestAPIKeyProfilesDeclareTheirRegistryKeyEnv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	var missing []string
	for _, p := range ListProviders(ctx) {
		row := registryLookup(ctx, p.Name)
		if p.AuthType != AuthTypeAPIKey || row == nil || row.AuthType != AuthTypeAPIKey || len(row.KeyEnvNames()) == 0 {
			continue
		}
		keys := p.KeyEnvNames()
		found := false
		for _, k := range row.KeyEnvNames() {
			if len(keys) > 0 && k == keys[0] {
				found = true
			}
		}
		if !found {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("profiles whose env vars miss their registry key: %v", missing)
	}
}

func TestBundledProfilesFollowPluginDirOrderAndResolveEveryName(t *testing.T) {
	var want []string
	for _, d := range bundledPluginDirs {
		want = append(want, d.names...)
	}
	if got := listedNames(BundledProfiles()); !reflect.DeepEqual(got, want) {
		t.Fatalf("bundled = %v\nwant %v", got, want)
	}
	p, _ := GetProviderProfile(context.Background(), "or")
	if p.Name != "openrouter" {
		t.Fatalf("openrouter alias = %q", p.Name)
	}
	if _, ok := GetProviderProfile(context.Background(), "openai-api"); ok {
		t.Fatal("a registry only id must have no profile")
	}
	if ProviderSource(context.Background(), "nous") != "bundled" {
		t.Fatal("bundled source missing")
	}
}

func TestMirroredRowIsAbsentInAHomeWithoutThePlugin(t *testing.T) {
	snapshotProviderRegistry(t)
	pluginHome, bareHome := t.TempDir(), t.TempDir()
	writeProviderPluginDir(t, pluginHome, "ambient")
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"ambient": func(string) ([]Profile, error) {
			return []Profile{{Name: "ambient-fake", DisplayName: "Ambient", BaseURL: "https://ambient-fake.invalid/v1",
				KeyEnv: []string{"AMBIENT_FAKE_API_KEY"}, RegistryAfter: "gemini"}}, nil
		},
	})
	t.Setenv("ICEBATH_HOME", pluginHome)
	ctx := context.Background()
	if row := registryLookup(ctx, "ambient-fake"); row == nil || row.BaseURL != "https://ambient-fake.invalid/v1" {
		t.Fatalf("plugin home row = %+v", row)
	}
	if ProviderSource(ctx, "ambient-fake") != "user" || !pluginMirroredProvider("ambient-fake") {
		t.Fatal("home plugin must mirror as a user row")
	}
	names := listedNames(profilesCtx(ctx))
	gem, amb := -1, -1
	for i, n := range names {
		switch n {
		case "gemini":
			gem = i
		case "ambient-fake":
			amb = i
		}
	}
	if gem < 0 || amb != gem+1 {
		t.Fatalf("registry_after ordering lost: %v", names)
	}
	t.Setenv("ICEBATH_HOME", bareHome)
	if row := registryLookup(ctx, "ambient-fake"); row != nil {
		t.Fatal("another home must not see the mirrored row")
	}
	if _, ok := ProfileFor("ambient-fake"); ok {
		t.Fatal("ProfileFor leaked the row across homes")
	}
	t.Setenv("ICEBATH_HOME", pluginHome)
	if _, ok := ProfileFor("ambient-fake"); !ok {
		t.Fatal("the plugin home lost its row")
	}
}

func TestHomePluginOverridesBuiltinRowOnlyInItsHome(t *testing.T) {
	snapshotProviderRegistry(t)
	pluginHome, bareHome := t.TempDir(), t.TempDir()
	writeProviderPluginDir(t, pluginHome, "deepseek-proxy")
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"deepseek-proxy": func(string) ([]Profile, error) {
			return []Profile{{Name: "deepseek", DisplayName: "DeepSeek Proxy", BaseURL: "https://proxy.invalid/v1",
				KeyEnv: []string{"DEEPSEEK_PROXY_KEY", "DEEPSEEK_PROXY_BASE_URL"}, Aliases: []string{"ds-proxy"}}}, nil
		},
	})
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", pluginHome)
	row := registryLookup(ctx, "deepseek")
	if row == nil || row.BaseURL != "https://proxy.invalid/v1" || row.DisplayName != "DeepSeek Proxy" ||
		!reflect.DeepEqual(row.KeyEnv, []string{"DEEPSEEK_PROXY_KEY"}) || row.BaseURLEnv != "DEEPSEEK_PROXY_BASE_URL" {
		t.Fatalf("home row = %+v", row)
	}
	if alias := registryLookup(ctx, "ds-proxy"); alias == nil || alias.Name != "deepseek" {
		t.Fatal("the home plugin alias must reach its row")
	}
	t.Setenv("ICEBATH_HOME", bareHome)
	if row := registryLookup(ctx, "deepseek"); row == nil || row.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("bare home row = %+v", row)
	}
	if alias := registryLookup(ctx, "ds-proxy"); alias == nil || alias.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("another home must read the built in row through the alias, got %+v", alias)
	}
}

func TestPluginLoadErrorIsRecordedAndClearedOnSuccess(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	writeProviderPluginDir(t, h, "broken")
	fail := true
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"broken": func(string) ([]Profile, error) {
			if fail {
				return nil, errors.New("boom")
			}
			return []Profile{{Name: "fixed-provider", AuthType: AuthTypeOAuthExternal}}, nil
		},
	})
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	if _, ok := GetProviderProfile(ctx, "fixed-provider"); ok {
		t.Fatal("a failed plugin registered a profile")
	}
	if got := PluginLoadError(ctx, "broken"); !strings.HasSuffix(got, ": boom") {
		t.Fatalf("load error = %q", got)
	}
	fail = false
	RefreshBoundHome(ctx)
	if _, ok := GetProviderProfile(ctx, "fixed-provider"); !ok {
		t.Fatal("the retried plugin did not register")
	}
	if got := PluginLoadError(ctx, "broken"); got != "" {
		t.Fatalf("load error kept after success = %q", got)
	}
}

func TestMissRecheckRunsOncePerNamePerTTL(t *testing.T) {
	layer := newHomeLayer()
	if !missRecheckDue(layer, "openai-api") || missRecheckDue(layer, "openai-api") {
		t.Fatal("a repeat miss inside the TTL must not force a check")
	}
	if !missRecheckDue(layer, "xai-oauth") {
		t.Fatal("a new name must force a check at once")
	}
	layer.misses["openai-api"] = time.Now().Add(-2 * pluginDirStampTTL)
	if !missRecheckDue(layer, "openai-api") {
		t.Fatal("a miss past the TTL must force a check")
	}
	for i := 0; i < 600; i++ {
		missRecheckDue(layer, "n"+strings.Repeat("x", i))
	}
	if len(layer.misses) > 512 {
		t.Fatalf("miss memo grew to %d", len(layer.misses))
	}
}

func TestDeclaresModelProviderKindReadsTheManifest(t *testing.T) {
	cases := map[string]bool{
		"kind: model-provider\nname: x\n":  true,
		"kind: \"model-provider\"\n":       true,
		"kind: general\n":                  false,
		"name: x\n":                        false,
		"kind: model-provider\n  bad: [\n": true,
	}
	for text, want := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := declaresModelProviderKind(dir); got != want {
			t.Fatalf("%q = %v", text, got)
		}
	}
	if declaresModelProviderKind(t.TempDir()) {
		t.Fatal("a dir without a manifest declares nothing")
	}
}

func TestProfileForHostMatchesSubdomainsAndMetadataHosts(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	RegisterProvider(Profile{Name: "acme", Hostnames: []string{"acme.example"}, MetadataHostnames: []string{"meta.acme.example"}})
	ctx := context.Background()
	if p, ok := ProfileForHost(ctx, "https://eu.acme.example/v1", false); !ok || p.Name != "acme" {
		t.Fatal("subdomain host not claimed")
	}
	if _, ok := ProfileForHost(ctx, "https://notacme.example/v1", false); ok {
		t.Fatal("a lookalike host was claimed")
	}
	if _, ok := ProfileForHost(ctx, "https://catalog.meta2.example", true); ok {
		t.Fatal("an unrelated metadata host was claimed")
	}
	if p, ok := ProfileForHost(ctx, "https://x.meta.acme.example", true); !ok || p.Name != "acme" {
		t.Fatal("metadata host not claimed")
	}
}

func TestRoutedModelRejectsVisionToolMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if !RoutedModelRejectsVisionToolMessages(ctx, "xiaomi", "mimo-v2.5") {
		t.Fatal("xiaomi rejects image tool parts")
	}
	if !RoutedModelRejectsVisionToolMessages(ctx, "openrouter", "xiaomi/mimo-v2.5") {
		t.Fatal("a routed xiaomi model rejects image tool parts")
	}
	if RoutedModelRejectsVisionToolMessages(ctx, "openrouter", "anthropic/claude") || RoutedModelRejectsVisionToolMessages(ctx, "openrouter", "bare") {
		t.Fatal("unrelated routes fail open")
	}
}

func TestLazyHeadersResolveOnceUnderConcurrentReads(t *testing.T) {
	var calls atomic.Int32
	inner := NewLazyHeaders(HeaderEntry{Key: "X-Inner", Fn: func() string { return "in" }})
	h := NewLazyHeaders(HeaderEntry{Key: "User-Agent", Fn: func() string {
		calls.Add(1)
		v, _ := inner.Get("X-Inner")
		return "ua-" + v
	}}, HeaderEntry{Key: "Accept", Value: "json"})
	if h.Len() != 2 || !h.Contains("User-Agent") || calls.Load() != 0 {
		t.Fatal("membership must not resolve")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.Get("User-Agent") }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("resolver ran %d times", calls.Load())
	}
	if v, _ := h.Get("User-Agent"); v != "ua-in" {
		t.Fatalf("value = %q", v)
	}
	if !h.Equal(map[string]string{"User-Agent": "ua-in", "Accept": "json"}) || h.String() != "{'User-Agent': 'ua-in', 'Accept': 'json'}" {
		t.Fatalf("repr = %s", h.String())
	}
	h.Set("X-New", "1")
	if k, v, ok := h.PopItem(); !ok || k != "X-New" || v != "1" {
		t.Fatal("popitem must pop the last insertion")
	}
	if h.SetDefault("Accept", "other") != "json" || h.PopDefault("Missing", "d") != "d" {
		t.Fatal("setdefault or pop default wrong")
	}
	if got := h.Or(map[string]string{"Accept": "xml"})["Accept"]; got != "xml" {
		t.Fatal("or must let the right side win")
	}
	if got := h.ROr(map[string]string{"Accept": "xml"})["Accept"]; got != "json" {
		t.Fatal("ror must let the headers win")
	}
}

func TestProfileHookDefaultsMatchTheBaseProfile(t *testing.T) {
	p := Profile{Name: "x", BaseURL: "https://API.Example.com/v1", DefaultMaxTokens: 4096, Hidden: false}
	if p.GetHostname() != "api.example.com" || p.GetMaxTokens("m") != 4096 || p.PreparePoolEntry(nil, "k") != "k" {
		t.Fatal("defaults wrong")
	}
	if !p.ResponsesReplayItemIDs() || !p.ResponsesPromptCacheKey() || !p.ResponsesTrustsCompletedStatus() || !p.ResponsesNativeCompaction() || p.ResponsesProfileReasoning() {
		t.Fatal("responses defaults wrong")
	}
	if c, err := p.CreateClient(context.Background(), nil); c != nil || err != nil {
		t.Fatal("create_client default must be nil")
	}
	if _, ok, err := p.SupportedReasoningEfforts(context.Background(), "m"); err != nil || ok || p.ReasoningPayload("m", Reasoning{}, "") != nil {
		t.Fatal("reasoning defaults wrong")
	}
	p.Hostname = "pinned.example"
	if p.GetHostname() != "pinned.example" {
		t.Fatal("declared hostname must win")
	}
	p.GetMaxTokensHook = func(string) int { return 7 }
	if p.GetMaxTokens("m") != 7 {
		t.Fatal("hook must override")
	}
	listed, err := Profile{Name: "dark", Hidden: true}.Listed(contextWithTempHome(t))
	if err != nil || listed {
		t.Fatal("a hidden profile without a pool row is unlisted")
	}
}

func contextWithTempHome(t *testing.T) context.Context {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func TestFetchModelsEndpointSelection(t *testing.T) {
	var seen []string
	var auth, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		auth, ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	p := Profile{Name: "acme", BaseURL: srv.URL + "/v1", ModelsURL: srv.URL + "/catalog/models", ModelListing: true,
		Headers: map[string]string{"User-Agent": "acme-ua"}}
	ids, ok := p.FetchModels(ctx, "k", srv.URL+"/v1/", 0)
	if !ok || !reflect.DeepEqual(ids, []string{"m1", "m2"}) || seen[0] != "/catalog/models" || auth != "Bearer k" || ua != "acme-ua" {
		t.Fatalf("default base: %v %v %v %q %q", ids, ok, seen, auth, ua)
	}
	if _, ok := p.FetchModels(ctx, "", srv.URL+"/proxy", 0); !ok || seen[1] != "/proxy/models" {
		t.Fatalf("custom base: %v", seen)
	}
	p.ModelsURL = ""
	if _, ok := p.FetchModels(ctx, "", "", 0); !ok || seen[2] != "/v1/models" {
		t.Fatalf("fallback: %v", seen)
	}
	p.ModelListing = false
	if _, ok := p.FetchModels(ctx, "", "", 0); ok {
		t.Fatal("listing off must return nothing")
	}
}

func TestHasNamedCustomProviderExportReadsTheHomeConfig(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\ncustom_providers:\n  - name: Local Box\n    base_url: http://10.0.0.5:8000/v1\n    api_key: sk-box\n")
	ctx := context.Background()
	if !HasNamedCustomProvider(ctx, h, "local-box") || HasNamedCustomProvider(ctx, h, "nowhere") {
		t.Fatal("named custom presence mismatch")
	}
	bad := runtimeConfigHome(t, "model: [unclosed\n")
	if HasNamedCustomProvider(ctx, bad, "local-box") {
		t.Fatal("malformed config reported a named provider")
	}
}
