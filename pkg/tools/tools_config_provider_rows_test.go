package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Plugin provider double with a schema, availability and model catalog
type fakePickerProvider struct {
	name, display             string
	schema                    map[string]any
	schemaPanics, availPanics bool
	available                 bool
	models                    []map[string]any
	def                       string
}

func (p *fakePickerProvider) Name() string        { return p.name }
func (p *fakePickerProvider) DisplayName() string { return p.display }
func (p *fakePickerProvider) GetSetupSchema() map[string]any {
	if p.schemaPanics {
		panic("schema boom")
	}
	return p.schema
}
func (p *fakePickerProvider) IsAvailable() bool {
	if p.availPanics {
		panic("availability boom")
	}
	return p.available
}
func (p *fakePickerProvider) ListModels() []map[string]any { return p.models }
func (p *fakePickerProvider) DefaultModel() string         { return p.def }

// Registry double listing the providers in order and finding them by name
func fakePickerRegistry(builtin map[string]bool, providers ...*fakePickerProvider) *PickerRegistry {
	return &PickerRegistry{
		List: func(context.Context, *Env) []PickerProvider {
			out := make([]PickerProvider, len(providers))
			for i, p := range providers {
				out[i] = p
			}
			return out
		},
		Get: func(_ context.Context, _ *Env, name string) PickerProvider {
			for _, p := range providers {
				if p.name == name {
					return p
				}
			}
			return nil
		},
		BuiltinNames: builtin,
	}
}

// Row as a plain map for comparisons in key order
func rowPairs(row *pyjson.Object) []string {
	var out []string
	for _, k := range row.Keys {
		v, _ := row.Get(k)
		b, _ := pyjson.EncodeWTF8(v)
		out = append(out, k+"="+string(b))
	}
	return out
}

func TestPluginProviderRowsShapesRegistryRows(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	paidVariant := map[string]any{"name": "Vendor · Paid", "badge": "paid", "web_tier": "paid",
		"env_vars": []any{map[string]any{"url": "https://v.example", "prompt": "Vendor key", "key": "VENDOR_KEY"}}}
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(map[string]bool{"edge": true},
		&fakePickerProvider{name: "", display: "Nameless", schema: map[string]any{"name": "x"}},
		&fakePickerProvider{name: " EDGE ", display: "Shadow", schema: map[string]any{"name": "shadow"}},
		&fakePickerProvider{name: "broken", display: "Broken", schemaPanics: true},
		&fakePickerProvider{name: "nil-schema", display: "Nil"},
		&fakePickerProvider{name: "vendor", display: "Vendor Display", schema: map[string]any{
			"tag": "Vendor voices", "post_setup": "piper", "web_tier": "free", "variants": []any{paidVariant, "skip"}}},
	))
	rows := s.pluginTTSProviders()
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	want := []string{`name="Vendor Display"`, `badge=""`, `tag="Vendor voices"`, `env_vars=[]`, `tts_provider="vendor"`, `tts_plugin_name="vendor"`, `post_setup="piper"`}
	if got := rowPairs(rows[0]); !slices.Equal(got, want) {
		t.Fatalf("row %v", got)
	}
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil,
		&fakePickerProvider{name: "fluxy", display: "Fluxy", schema: map[string]any{"name": "Fluxy Images", "badge": "paid"}},
		&fakePickerProvider{name: "", display: "Unnamed", schema: map[string]any{"name": "Unnamed"}},
	))
	rows = s.pluginImageGenProviders()
	if len(rows) != 2 || rowGet(rows[0], "image_gen_plugin_name") != "fluxy" || rowGet(rows[1], "image_gen_plugin_name") != "" {
		t.Fatalf("image rows %v", rows)
	}
	saved := webPickerRegistry
	t.Cleanup(func() { webPickerRegistry = saved })
	webPickerRegistry = fakePickerRegistry(nil, &fakePickerProvider{name: "vendor", display: "Vendor", schema: map[string]any{
		"name": "Vendor · Free", "web_tier": "free", "variants": []any{paidVariant, map[string]any{"name": "Untiered"}}}})
	rows = s.pluginWebSearchProviders()
	var names, tiers []string
	for _, r := range rows {
		names = append(names, rowText(r, "name"))
		tiers = append(tiers, pyOrStr(rowGet(r, "web_tier")))
		if rowGet(r, "web_backend") != "vendor" || rowGet(r, "web_search_plugin_name") != "vendor" {
			t.Fatalf("web markers %v", rowPairs(r))
		}
	}
	if !slices.Equal(names, []string{"Vendor · Free", "Vendor · Paid", "Untiered"}) || !slices.Equal(tiers, []string{"free", "paid", ""}) {
		t.Fatalf("web rows %v %v", names, tiers)
	}
	envRow, _ := rows[1].Get("env_vars")
	entry := envRow.([]any)[0].(*pyjson.Object)
	if !slices.Equal(entry.Keys, []string{"key", "prompt", "url"}) {
		t.Fatalf("env entry order %v", entry.Keys)
	}
}

func TestPluginProviderRowsFailuresReturnNoRows(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, nil)
	if rows := s.pluginVideoGenProviders(); len(rows) != 0 {
		t.Fatalf("unregistered module rows %v", rows)
	}
	reg := fakePickerRegistry(nil, &fakePickerProvider{name: "v", schema: map[string]any{"name": "V"}})
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, reg)
	swapHook(t, &EnsurePluginsDiscovered, func(context.Context) error { return errors.New("discovery failed") })
	if rows := s.pluginVideoGenProviders(); len(rows) != 0 {
		t.Fatalf("rows after failed discovery %v", rows)
	}
	swapHook(t, &EnsurePluginsDiscovered, nil)
	if rows := s.pluginVideoGenProviders(); len(rows) != 0 {
		t.Fatalf("rows without discovery %v", rows)
	}
	swapHook(t, &EnsurePluginsDiscovered, func(context.Context) error { return nil })
	reg.List = func(context.Context, *Env) []PickerProvider { panic("list boom") }
	if rows := s.pluginVideoGenProviders(); len(rows) != 0 {
		t.Fatalf("rows after failed listing %v", rows)
	}
	swapHook(t, &ToolsConfigHooks.TTSRegistry, &PickerRegistry{List: func(context.Context, *Env) []PickerProvider { return nil }})
	if rows := s.pluginTTSProviders(); len(rows) != 0 {
		t.Fatalf("tts rows without builtin names %v", rows)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "not iterable") {
			t.Fatalf("variants of an int recovered %v", r)
		}
	}()
	saved := webPickerRegistry
	t.Cleanup(func() { webPickerRegistry = saved })
	webPickerRegistry = fakePickerRegistry(nil, &fakePickerProvider{name: "odd", schema: map[string]any{"variants": 3}})
	s.pluginWebSearchProviders()
}

func TestBrowserPluginRowsCarryLegacyMarkers(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.BrowserRegistry, fakePickerRegistry(nil,
		&fakePickerProvider{name: "browserbase", display: "Browserbase", schema: map[string]any{"name": "Browserbase", "post_setup": "browserbase",
			"env_vars": []map[string]any{{"key": "BROWSERBASE_API_KEY", "prompt": "Browserbase API key"}}}}))
	rows := s.pluginBrowserProviders()
	want := []string{`name="Browserbase"`, `badge=""`, `tag=""`, `env_vars=[{"key":"BROWSERBASE_API_KEY","prompt":"Browserbase API key"}]`,
		`browser_provider="browserbase"`, `browser_plugin_name="browserbase"`, `post_setup="browserbase"`}
	if len(rows) != 1 || !slices.Equal(rowPairs(rows[0]), want) {
		t.Fatalf("browser rows %v", rows)
	}
}

func TestWebProviderCapabilitiesFollowTheRegistry(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	if got := s.WebProviderCapabilities("ddgs"); !slices.Equal(got, []string{"search"}) {
		t.Fatalf("ddgs %v", got)
	}
	if got := s.WebProviderCapabilities("firecrawl"); !slices.Equal(got, []string{"search", "extract"}) {
		t.Fatalf("firecrawl %v", got)
	}
	if got := s.WebProviderCapabilities("no-such-backend"); !slices.Equal(got, []string{"search", "extract"}) {
		t.Fatalf("unknown %v", got)
	}
}

func TestComputerUseRowsSkipTheBuiltinBackend(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ComputerUseDefaultBackend, func() string { return "cua" })
	swapHook(t, &ToolsConfigHooks.ComputerUseProviders, func(*Env) ([][2]string, error) {
		return [][2]string{{"cua", "builtin"}, {"remote-desk", "Remote desktop driver"}}, nil
	})
	rows, err := s.computerUseProviderRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	want := []string{`name="remote-desk"`, `badge=""`, `tag="Remote desktop driver"`, `env_vars=[]`, `computer_use_backend="remote-desk"`}
	if !slices.Equal(rowPairs(rows[0]), want) {
		t.Fatalf("row %v", rowPairs(rows[0]))
	}
	swapHook(t, &ToolsConfigHooks.ComputerUseProviders, func(*Env) ([][2]string, error) { return nil, errors.New("plugins dir unreadable") })
	if _, err := s.VisibleProviders(catalogCategory("computer_use"), config.Tree{}, false, &NousToolFeatures{}); err == nil {
		t.Fatal("discovery failure swallowed")
	}
}

// Nous features double with auth, the per feature states and an account
func nousFeaturesDouble(auth bool, features map[string]NousToolFeature, account *NousToolAccount) func(context.Context, string, map[string]any, bool) NousToolFeatures {
	return func(context.Context, string, map[string]any, bool) NousToolFeatures {
		return NousToolFeatures{NousAuthPresent: auth, Features: features, Account: account}
	}
}

// Account double entitled for the listed coverage categories
func accountDouble(loggedIn bool, paid *bool, entitled bool, categories ...string) *NousToolAccount {
	return &NousToolAccount{LoggedIn: loggedIn, PaidServiceAccess: paid, ToolGatewayEntitled: entitled,
		EntitledFor: func(c string) bool { return (paid != nil && *paid) || slices.Contains(categories, c) }}
}

func boolRef(b bool) *bool { return &b }

func TestVisibleProvidersGateNousRows(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	cat := toolCategory("name", "Custom", "providers", []any{
		toolRow("Nous only", "", "", nil, "requires_nous_auth", true),
		toolRow("Managed video", "", "", nil, "requires_nous_auth", true, "managed_nous_feature", "video_gen"),
		toolRow("Plain", "", "", nil),
	})
	names := func(f *NousToolFeatures) []string {
		rows, err := s.VisibleProviders(cat, config.Tree{}, false, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, rowText(r, "name"))
		}
		return out
	}
	if got := names(&NousToolFeatures{}); !slices.Equal(got, []string{"Managed video", "Plain"}) {
		t.Fatalf("logged out %v", got)
	}
	if got := names(&NousToolFeatures{NousAuthPresent: true}); !slices.Equal(got, []string{"Nous only", "Managed video", "Plain"}) {
		t.Fatalf("logged in %v", got)
	}
	pool := &NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, boolRef(false), true)}
	if got := names(pool); !slices.Equal(got, []string{"Nous only", "Plain"}) {
		t.Fatalf("pool only %v", got)
	}
	pool.Account = accountDouble(true, nil, true, "fal-video")
	if got := names(pool); !slices.Equal(got, []string{"Nous only", "Managed video", "Plain"}) {
		t.Fatalf("pool funded for video %v", got)
	}
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, nil, nil))
	if got := names(nil); !slices.Equal(got, []string{"Nous only", "Managed video", "Plain"}) {
		t.Fatalf("fetched features %v", got)
	}
}

func TestProviderReadinessStatusVerdicts(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "READY_KEY", "XAI_API_KEY")
	keyed := toolRow("Keyed", "", "", []any{toolKey("READY_KEY", "Ready key", "")})
	status := func(row *pyjson.Object, f *NousToolFeatures, active *bool) string {
		got, err := s.ProviderReadinessStatus(row, config.Tree{}, f, active)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := status(keyed, nil, nil); got != "needs_keys" {
		t.Fatalf("keyed %s", got)
	}
	writeHomeEnv(t, s.home(), map[string]string{"READY_KEY": "v"})
	if got := status(keyed, nil, nil); got != "ready" {
		t.Fatalf("keyed with key %s", got)
	}
	swapHook(t, &ToolsConfigHooks.ManagedCoverageCategory, func(f string) (string, bool) {
		if f == "image_gen" {
			return "fal-image", true
		}
		return "", false
	})
	nousOnly := toolRow("Nous", "", "", nil, "requires_nous_auth", true)
	if got := status(nousOnly, &NousToolFeatures{}, nil); got != "needs_auth" {
		t.Fatalf("logged out %s", got)
	}
	if got := status(nousOnly, &NousToolFeatures{NousAuthPresent: true}, nil); got != "ready" {
		t.Fatalf("logged in %s", got)
	}
	managedImage := toolRow("Managed image", "", "", nil, "managed_nous_feature", "image_gen")
	pool := &NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, nil, true)}
	if got := status(managedImage, pool, nil); got != "needs_auth" {
		t.Fatalf("pool without image coverage %s", got)
	}
	pool.Account = accountDouble(true, nil, true, "fal-image")
	if got := status(managedImage, pool, nil); got != "ready" {
		t.Fatalf("pool with image coverage %s", got)
	}
	managedWeb := toolRow("Managed web", "", "", nil, "managed_nous_feature", "web")
	if got := status(managedWeb, &NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, nil, false)}, nil); got != "needs_auth" {
		t.Fatalf("unentitled web %s", got)
	}
	if got := status(managedWeb, &NousToolFeatures{NousAuthPresent: true, Account: accountDouble(false, nil, true)}, nil); got != "needs_auth" {
		t.Fatalf("logged out account %s", got)
	}
	if got := status(managedWeb, &NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, nil, true)}, nil); got != "ready" {
		t.Fatalf("entitled web %s", got)
	}
	xai := toolRow("xAI", "", "", nil, "post_setup", "xai_grok")
	swapHook(t, &XAIOAuthTokensReader, func(context.Context, string) error { return errors.New("no tokens") })
	if got := status(xai, nil, nil); got != "needs_auth" {
		t.Fatalf("xai without credentials %s", got)
	}
	swapHook(t, &XAIOAuthTokensReader, func(context.Context, string) error { return nil })
	if got := status(xai, nil, nil); got != "ready" {
		t.Fatalf("xai with oauth %s", got)
	}
	cua := toolRow("cua", "", "", nil, "computer_use_backend", "cua", "post_setup", "cua_driver")
	for _, c := range []struct {
		ready bool
		err   error
		want  string
	}{{true, nil, "ready"}, {false, nil, "needs_setup"}, {false, errors.New("probe failed"), "ready"}} {
		swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return c.ready, c.err })
		if got := status(cua, nil, nil); got != c.want {
			t.Fatalf("cua %+v got %s", c, got)
		}
	}
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { panic("probe crashed") })
	if got := status(cua, nil, nil); got != "ready" {
		t.Fatalf("crashing probe %s", got)
	}
	unregistered := toolRow("Hooked", "", "", nil, "tts_provider", "kittentts-plus", "post_setup", "custom_hook")
	if got := status(unregistered, nil, boolRef(true)); got != "ready" {
		t.Fatalf("active unregistered hook %s", got)
	}
	if got := status(unregistered, nil, boolRef(false)); got != "needs_setup" {
		t.Fatalf("inactive unregistered hook %s", got)
	}
	cfgActive := config.Tree{"tts": map[string]any{"provider": "kittentts-plus"}}
	if got, err := s.ProviderReadinessStatus(unregistered, cfgActive, nil, nil); err != nil || got != "ready" {
		t.Fatalf("computed active %s %v", got, err)
	}
	if got := status(toolRow("Plain", "", "", nil), nil, nil); got != "ready" {
		t.Fatalf("plain %s", got)
	}
}

func TestToolsetNeedsConfigurationPromptSignals(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "FAL_KEY")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, nil, nil))
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return false, nil })
	swapHook(t, &ToolsConfigHooks.ComputerUseDefaultBackend, func() string { return "cua" })
	swapHook(t, &ToolsConfigHooks.ComputerUseProviders, func(*Env) ([][2]string, error) { return nil, nil })
	needs := func(key string, cfg config.Tree) bool {
		got, err := s.toolsetNeedsConfigurationPrompt(key, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !needs("computer_use", config.Tree{}) {
		t.Fatal("missing cua-driver did not force setup")
	}
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(map[string]bool{}))
	swapHook(t, &ToolsConfigHooks.BrowserRegistry, fakePickerRegistry(nil))
	if !needs("tts", config.Tree{}) || needs("tts", config.Tree{"tts": map[string]any{"provider": "edge"}}) || !needs("tts", config.Tree{"tts": "edge"}) {
		t.Fatal("tts selection signal wrong")
	}
	if !needs("web", config.Tree{"web": map[string]any{}}) || needs("web", config.Tree{"web": map[string]any{"backend": nil}}) {
		t.Fatal("web selection signal wrong")
	}
	if !needs("browser", config.Tree{}) || needs("browser", config.Tree{"browser": map[string]any{"cloud_provider": "local"}}) {
		t.Fatal("browser selection signal wrong")
	}
	image := &fakePickerProvider{name: "fluxy", schema: map[string]any{"name": "Fluxy"}}
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, image))
	if !needs("image_gen", config.Tree{}) {
		t.Fatal("image gen without FAL key or available plugin skipped setup")
	}
	image.available = true
	if needs("image_gen", config.Tree{}) {
		t.Fatal("available plugin image provider still prompts")
	}
	image.available = false
	writeHomeEnv(t, s.home(), map[string]string{"FAL_KEY": "fal-x"})
	s.Env.Secrets = config.NewSecrets(s.home())
	if needs("image_gen", config.Tree{}) {
		t.Fatal("FAL key still prompts")
	}
	video := &fakePickerProvider{name: "veo", schema: map[string]any{"name": "Veo"}, availPanics: true}
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, video))
	if !needs("video_gen", config.Tree{}) {
		t.Fatal("crashing video availability counted as available")
	}
	video.availPanics, video.available = false, true
	if needs("video_gen", config.Tree{}) {
		t.Fatal("available video provider still prompts")
	}
}

func TestWebTierMatchesConfiguredAndAutoTiers(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	paid, free := webTestRow("exa", "web_tier", "paid"), webTestRow("exa", "web_tier", "free")
	if !s.webTierMatches(webTestRow("exa"), config.Tree{}) {
		t.Fatal("untiered row mismatched")
	}
	pinned := config.Tree{"web": map[string]any{"provider_tier": map[string]any{"exa": " PAID "}}}
	if !s.webTierMatches(paid, pinned) || s.webTierMatches(free, pinned) {
		t.Fatal("pinned paid tier wrong")
	}
	var asked []string
	swapHook(t, &webTierProviderEnv, func(_ context.Context, _ *Env, name string) (string, error) {
		asked = append(asked, name)
		return "exa-key", nil
	})
	auto := config.Tree{"web": "not a dict"}
	if !s.webTierMatches(paid, auto) || s.webTierMatches(free, auto) || !slices.Equal(asked, []string{"EXA_API_KEY", "EXA_API_KEY"}) {
		t.Fatalf("auto with key wrong, asked %v", asked)
	}
	swapHook(t, &webTierProviderEnv, func(context.Context, *Env, string) (string, error) { return "", ErrUnscopedSecret })
	if s.webTierMatches(paid, config.Tree{}) || !s.webTierMatches(free, config.Tree{}) {
		t.Fatal("lookup failure did not read as keyless")
	}
	asked = nil
	swapHook(t, &webTierProviderEnv, func(_ context.Context, _ *Env, name string) (string, error) {
		asked = append(asked, name)
		return "k", nil
	})
	if !s.webTierMatches(webTestRow("tavily", "web_tier", "free"), config.Tree{}) || len(asked) != 0 {
		t.Fatalf("vendor without a tier key asked %v", asked)
	}
}

func TestWebServingBackendsReadsTheLegacyGateway(t *testing.T) {
	got := webServingBackends(config.Tree{"web": map[string]any{"use_gateway": "yes", "backend": "exa", "extract_backend": " Tavily "}})
	if len(got) != 2 || !got["nous"] || !got["tavily"] {
		t.Fatalf("serving %v", got)
	}
	if got := webServingBackends(config.Tree{}); len(got) != 0 {
		t.Fatalf("empty config serving %v", got)
	}
}

func TestManagedProviderActiveSelections(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	managed := map[string]NousToolFeature{"image_gen": {ManagedByNous: true}, "video_gen": {ManagedByNous: true},
		"tts": {ManagedByNous: true}, "browser": {ManagedByNous: true}}
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, managed, nil))
	active := func(row *pyjson.Object, cfg config.Tree) bool {
		t.Helper()
		got, err := s.IsProviderActive(row, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	image := toolRow("Nous", "", "", nil, "managed_nous_feature", "image_gen", "imagegen_backend", "nous")
	for cfg, want := range map[string]bool{"": true, "fal": true, "nous": true, "openai": false} {
		if got := active(image, config.Tree{"image_gen": map[string]any{"provider": cfg}}); got != want {
			t.Fatalf("image provider %q active %v", cfg, got)
		}
	}
	if active(image, config.Tree{"image_gen": map[string]any{"provider": "fal", "use_gateway": false}}) {
		t.Fatal("direct FAL with use_gateway false read as managed")
	}
	if !active(image, config.Tree{"image_gen": map[string]any{"provider": "nous", "use_gateway": false}}) {
		t.Fatal("explicit nous pick ignored")
	}
	if !active(image, config.Tree{"image_gen": "corrupt"}) {
		t.Fatal("non dict image section blocked the managed state")
	}
	if _, err := s.IsProviderActive(image, config.Tree{"image_gen": map[string]any{"provider": []any{"x"}}}, false); err == nil {
		t.Fatal("unhashable provider accepted")
	}
	tts := toolRow("Nous TTS", "", "", nil, "tts_provider", "openai", "managed_nous_feature", "tts")
	if !active(tts, config.Tree{"tts": map[string]any{"provider": "nous"}}) || !active(tts, config.Tree{"tts": map[string]any{"provider": "openai"}}) ||
		active(tts, config.Tree{"tts": map[string]any{"provider": "edge"}}) {
		t.Fatal("managed tts selection wrong")
	}
	browser := toolRow("Nous browser", "", "", nil, "browser_provider", "browser-use", "managed_nous_feature", "browser")
	if !active(browser, config.Tree{"browser": map[string]any{"cloud_provider": "browser-use"}}) || active(browser, config.Tree{}) {
		t.Fatal("managed browser selection wrong")
	}
	stt := toolRow("Nous STT", "", "", nil, "stt_provider", "openai", "managed_nous_feature", "stt")
	if active(stt, config.Tree{"stt": map[string]any{"provider": "nous"}}) {
		t.Fatal("missing managed feature read active")
	}
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, map[string]NousToolFeature{"tts": {}}, nil))
	if active(tts, config.Tree{"tts": map[string]any{"provider": []any{"unhashable"}}}) {
		t.Fatal("unmanaged feature read active")
	}
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, map[string]NousToolFeature{"modal": {ManagedByNous: true}}, nil))
	if !active(toolRow("Modal", "", "", nil, "managed_nous_feature", "modal"), config.Tree{}) {
		t.Fatal("marker free managed row inactive")
	}
}

func TestUnmanagedActiveChecks(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	active := func(row *pyjson.Object, cfg config.Tree) bool {
		t.Helper()
		got, err := s.IsProviderActive(row, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	plugin := toolRow("Fluxy", "", "", nil, "image_gen_plugin_name", "fluxy")
	if !active(plugin, config.Tree{"image_gen": map[string]any{"provider": "fluxy"}}) ||
		active(plugin, config.Tree{"image_gen": map[string]any{"provider": "fluxy", "use_gateway": true}}) || active(plugin, config.Tree{}) {
		t.Fatal("plugin image row wrong")
	}
	video := toolRow("Veo", "", "", nil, "video_gen_plugin_name", "veo")
	if !active(video, config.Tree{"video_gen": map[string]any{"provider": "veo", "use_gateway": true}}) {
		t.Fatal("plugin video row ignores use_gateway")
	}
	local := toolRow("Local Whisper", "", "", nil, "stt_provider", "local")
	if !active(local, config.Tree{}) || active(local, config.Tree{"stt": map[string]any{"provider": "groq"}}) {
		t.Fatal("stt default wrong")
	}
	lightpanda := toolRow("Lightpanda", "", "", nil, "browser_provider", "local", "browser_engine", "lightpanda")
	chromium := toolRow("Local Browser", "", "", nil, "browser_provider", "local", "browser_engine", "auto")
	cfg := config.Tree{"browser": map[string]any{"cloud_provider": "local"}}
	if !active(chromium, cfg) || active(lightpanda, cfg) {
		t.Fatal("default engine wrong")
	}
	cfg["browser"].(map[string]any)["engine"] = " LightPanda "
	if active(chromium, cfg) || !active(lightpanda, cfg) {
		t.Fatal("lightpanda engine wrong")
	}
	if active(toolRow("Cloud", "", "", nil, "browser_provider", "browserbase"), cfg) {
		t.Fatal("other cloud provider active")
	}
	cu := toolRow("cua", "", "", nil, "computer_use_backend", "cua")
	if !active(cu, config.Tree{"computer_use": map[string]any{"backend": "cua"}}) || active(cu, config.Tree{}) {
		t.Fatal("computer use row wrong")
	}
	fal := toolRow("FAL", "", "", nil, "imagegen_backend", "fal")
	if !active(fal, config.Tree{}) || active(fal, config.Tree{"image_gen": map[string]any{"use_gateway": "on"}}) ||
		active(fal, config.Tree{"image_gen": map[string]any{"provider": "openai"}}) || active(toolRow("N", "", "", nil, "imagegen_backend", "nous"), config.Tree{}) {
		t.Fatal("FAL backend row wrong")
	}
	if active(toolRow("Markerless", "", "", nil), config.Tree{}) {
		t.Fatal("markerless row active")
	}
}

func TestDetectActiveProviderIndexFallsBackToConfiguredKeys(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "SECOND_KEY")
	rows := []*pyjson.Object{
		toolRow("Edge", "", "", nil, "tts_provider", "edge"),
		toolRow("Keyed", "", "", []any{toolKey("SECOND_KEY", "Second", "")}, "tts_provider", "keyed"),
	}
	idx, err := s.detectActiveProviderIndex(rows, config.Tree{}, false)
	if err != nil || idx != 0 {
		t.Fatalf("nothing active %d %v", idx, err)
	}
	writeHomeEnv(t, s.home(), map[string]string{"SECOND_KEY": "x"})
	if idx, err = s.detectActiveProviderIndex(rows, config.Tree{}, false); err != nil || idx != 1 {
		t.Fatalf("keyed fallback %d %v", idx, err)
	}
	if idx, err = s.detectActiveProviderIndex(rows, config.Tree{"tts": map[string]any{"provider": "edge"}}, false); err != nil || idx != 0 {
		t.Fatalf("active first %d %v", idx, err)
	}
}

func TestModelCatalogSources(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.FALModels, func() ([]string, map[string]map[string]any, string) {
		return []string{"fal-a", "fal-b"}, map[string]map[string]any{"fal-a": {"speed": "1s"}, "fal-b": {}}, "fal-b"
	})
	cat, def := s.falModelCatalog(config.Tree{})
	if !slices.Equal(cat.ids, []string{"fal-a", "fal-b"}) || def != "fal-b" {
		t.Fatalf("fal catalog %v %s", cat.ids, def)
	}
	var gates [][2]bool
	swapHook(t, &ToolsConfigHooks.ManagedImageCatalog, func(_ context.Context, _ *Env, krea, portal bool) ([]string, map[string]map[string]any, string) {
		gates = append(gates, [2]bool{krea, portal})
		return []string{"m"}, map[string]map[string]any{"m": {}}, "m"
	})
	for _, acct := range []*NousToolAccount{nil, accountDouble(true, boolRef(true), true), accountDouble(true, boolRef(false), true), accountDouble(true, nil, true, "krea")} {
		swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, nil, acct))
		s.managedImageCatalog(config.Tree{})
	}
	if !slices.Equal(gates, [][2]bool{{true, true}, {true, true}, {false, false}, {true, false}}) {
		t.Fatalf("managed gates %v", gates)
	}
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "fluxy", def: "fluxy-2",
		models: []map[string]any{{"id": "fluxy-1", "speed": "fast"}, {"name": "no id"}, nil, {"id": "fluxy-2"}, {"id": "fluxy-1", "speed": "slow"}}}))
	cat, def = s.pluginImageGenCatalog("fluxy")
	if !slices.Equal(cat.ids, []string{"fluxy-1", "fluxy-2"}) || cat.meta["fluxy-1"]["speed"] != "slow" || def != "fluxy-2" {
		t.Fatalf("plugin catalog %v %v %s", cat.ids, cat.meta, def)
	}
	if cat, def = s.pluginImageGenCatalog("missing"); len(cat.ids) != 0 || def != "" {
		t.Fatalf("unknown plugin catalog %v %s", cat.ids, def)
	}
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, nil)
	if cat, def = s.pluginVideoGenCatalog("veo"); len(cat.ids) != 0 || def != "" {
		t.Fatalf("missing registry catalog %v %s", cat.ids, def)
	}
}

func TestWriteProviderConfigPersistsOneSelection(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	write := func(row *pyjson.Object, cfg config.Tree, managed string) config.Tree {
		t.Helper()
		if err := s.writeProviderConfig(row, cfg, managed); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := write(toolRow("Edge", "", "", nil, "tts_provider", "edge"), config.Tree{"tts": map[string]any{"use_gateway": true}}, "")
	if !pyjson.Equal(cfg["tts"], map[string]any{"provider": "edge"}) {
		t.Fatalf("tts %v", cfg["tts"])
	}
	cfg = write(toolRow("Nous STT", "", "", nil, "stt_provider", "openai", "managed_nous_feature", "stt"), config.Tree{}, "stt")
	if !pyjson.Equal(cfg["stt"], map[string]any{"provider": "nous"}) {
		t.Fatalf("stt %v", cfg["stt"])
	}
	cfg = write(toolRow("Local", "", "", nil, "browser_provider", "", "browser_engine", "lightpanda"), config.Tree{"browser": map[string]any{"use_gateway": true}}, "")
	if !pyjson.Equal(cfg["browser"], map[string]any{"engine": "lightpanda"}) {
		t.Fatalf("browser %v", cfg["browser"])
	}
	if err := s.writeProviderConfig(toolRow("Local", "", "", nil, "browser_provider", ""), config.Tree{"browser": nil}, ""); err == nil ||
		err.Error() != "AttributeError: 'NoneType' object has no attribute 'pop'" {
		t.Fatalf("pop on None %v", err)
	}
	if err := s.writeProviderConfig(toolRow("L", "", "", nil, "browser_engine", "auto"), config.Tree{"browser": "x"}, ""); err == nil ||
		err.Error() != "TypeError: 'str' object does not support item assignment" {
		t.Fatalf("item assignment on str %v", err)
	}
	cfg = write(toolRow("Exa paid", "", "", nil, "web_backend", "exa", "web_tier", "paid"),
		config.Tree{"web": map[string]any{"search_backend": "tavily"}}, "")
	if !pyjson.Equal(cfg["web"], map[string]any{"backend": "exa", "search_backend": "tavily", "provider_tier": map[string]any{"exa": "paid"}}) {
		t.Fatalf("tiered web %v", cfg["web"])
	}
	cfg = write(toolRow("Exa", "", "", nil, "web_backend", "exa"), cfg, "")
	if !pyjson.Equal(cfg["web"], map[string]any{"backend": "exa", "search_backend": "tavily", "provider_tier": map[string]any{}}) {
		t.Fatalf("untiered web %v", cfg["web"])
	}
	cfg = write(toolRow("Nous web", "", "", nil, "web_backend", "firecrawl", "managed_nous_feature", "web"),
		config.Tree{"web": map[string]any{"search_backend": "tavily", "extract_backend": "exa", "use_gateway": true}}, "web")
	if !pyjson.Equal(cfg["web"], map[string]any{"backend": "nous"}) {
		t.Fatalf("managed web %v", cfg["web"])
	}
	cfg = write(toolRow("cua", "", "", nil, "computer_use_backend", "cua"), config.Tree{}, "")
	if !pyjson.Equal(cfg["computer_use"], map[string]any{"backend": "cua"}) {
		t.Fatalf("computer use %v", cfg["computer_use"])
	}
	cfg = write(toolRow("Nous video", "", "", nil, "managed_nous_feature", "video_gen"), config.Tree{"video_gen": map[string]any{"use_gateway": true, "provider": "veo"}}, "video_gen")
	if !pyjson.Equal(cfg["video_gen"], map[string]any{"provider": "nous"}) {
		t.Fatalf("managed video %v", cfg["video_gen"])
	}
	cfg = write(toolRow("Nous image", "", "", nil, "managed_nous_feature", "image_gen"), config.Tree{"image_gen": "kept"}, "image_gen")
	if cfg["image_gen"] != "kept" {
		t.Fatalf("non dict managed section %v", cfg["image_gen"])
	}
	xai := catalogCategory("x_search").GetOr("providers", nil).([]any)[1].(*pyjson.Object)
	cfg = write(xai, config.Tree{"x_search": map[string]any{"use_gateway": true}}, "")
	if !pyjson.Equal(cfg["x_search"], map[string]any{}) {
		t.Fatalf("catalog membership fallback %v", cfg["x_search"])
	}
	cfg = write(toolRow("Fluxy", "", "", nil, "image_gen_plugin_name", "fluxy"), config.Tree{"image_gen": map[string]any{"use_gateway": true}}, "")
	if !pyjson.Equal(cfg["image_gen"], map[string]any{}) {
		t.Fatalf("plugin row marker section %v", cfg["image_gen"])
	}
}

func TestApplyProviderSelectionResolvesVisibleRows(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, nil, nil))
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "fluxy", display: "Fluxy", schema: map[string]any{"name": "Fluxy"}}))
	err := s.ApplyProviderSelection("nope", "x", config.Tree{})
	var sel *ProviderSelectionError
	if !errors.As(err, &sel) || sel.Message != "Toolset has no configurable category: nope" || err.Error() != "'Toolset has no configurable category: nope'" {
		t.Fatalf("unknown toolset %v", err)
	}
	err = s.ApplyProviderSelection("image_gen", "Missing", config.Tree{})
	if !errors.As(err, &sel) || sel.Message != "Unknown provider 'Missing' for toolset 'image_gen'" || err.Error() != `"Unknown provider 'Missing' for toolset 'image_gen'"` {
		t.Fatalf("unknown provider %v", err)
	}
	cfg := config.Tree{"image_gen": map[string]any{"use_gateway": true}}
	if err := s.ApplyProviderSelection("image_gen", "Fluxy", cfg); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["image_gen"], map[string]any{"provider": "fluxy"}) {
		t.Fatalf("plugin image pick %v", cfg["image_gen"])
	}
	if err := s.ApplyProviderSelection("image_gen", "Nous Subscription", cfg); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["image_gen"], map[string]any{"provider": "nous"}) {
		t.Fatalf("managed image pick %v", cfg["image_gen"])
	}
	saved := ToolCategories
	t.Cleanup(func() { ToolCategories = saved })
	ToolCategories = toolCategory("image_gen", toolCategory("name", "Image Generation", "providers", []any{
		toolRow("FAL.ai", "", "", nil, "imagegen_backend", "fal"),
	}))
	if err := s.ApplyProviderSelection("image_gen", "FAL.ai", cfg); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["image_gen"], map[string]any{"provider": "fal"}) {
		t.Fatalf("FAL pick %v", cfg["image_gen"])
	}
}
