package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
func rowPairs(row *jsonutil.Object) []string {
	var out []string
	for _, k := range row.Keys() {
		v, _ := row.Get(k)
		b, _ := jsonutil.Append(nil, v)
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
	entry := envRow.([]any)[0].(*jsonutil.Object)
	if !slices.Equal(entry.Keys(), []string{"key", "prompt", "url"}) {
		t.Fatalf("env entry order %v", entry.Keys())
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
		if r := recover(); r == nil || !strings.Contains(r.(string), "expected a list of mappings") {
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
	if _, err := s.VisibleProviders(catalogCategory("computer_use")); err == nil {
		t.Fatal("discovery failure swallowed")
	}
}

func TestToolsetNeedsConfigurationPromptSignals(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "FAL_KEY")
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return false, nil })
	swapHook(t, &ToolsConfigHooks.ComputerUseDefaultBackend, func() string { return "cua" })
	swapHook(t, &ToolsConfigHooks.ComputerUseProviders, func(*Env) ([][2]string, error) { return nil, nil })
	needs := func(key string, cfg config.Tree) bool {
		got, err := s.toolsetNeedsConfigurationPrompt(key, cfg)
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

func TestWebServingBackendsReadsTheSharedAndPinnedBackends(t *testing.T) {
	got := webServingBackends(config.Tree{"web": map[string]any{"backend": "exa", "extract_backend": " Tavily "}})
	if len(got) != 2 || !got["exa"] || !got["tavily"] {
		t.Fatalf("serving %v", got)
	}
	if got := webServingBackends(config.Tree{}); len(got) != 0 {
		t.Fatalf("empty config serving %v", got)
	}
}

func TestUnmanagedActiveChecks(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	active := func(row *jsonutil.Object, cfg config.Tree) bool {
		t.Helper()
		got, err := s.IsProviderActive(row, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	plugin := toolRow("Fluxy", "", "", nil, "image_gen_plugin_name", "fluxy")
	if !active(plugin, config.Tree{"image_gen": map[string]any{"provider": "fluxy"}}) || active(plugin, config.Tree{}) {
		t.Fatal("plugin image row wrong")
	}
	video := toolRow("Veo", "", "", nil, "video_gen_plugin_name", "veo")
	if !active(video, config.Tree{"video_gen": map[string]any{"provider": "veo"}}) {
		t.Fatal("plugin video row inactive")
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
	if !active(fal, config.Tree{}) || active(fal, config.Tree{"image_gen": map[string]any{"provider": "openai"}}) {
		t.Fatal("FAL backend row wrong")
	}
	if active(toolRow("Markerless", "", "", nil), config.Tree{}) {
		t.Fatal("markerless row active")
	}
}

func TestDetectActiveProviderIndexFallsBackToConfiguredKeys(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "SECOND_KEY")
	rows := []*jsonutil.Object{
		toolRow("Edge", "", "", nil, "tts_provider", "edge"),
		toolRow("Keyed", "", "", []any{toolKey("SECOND_KEY", "Second", "")}, "tts_provider", "keyed"),
	}
	idx, err := s.detectActiveProviderIndex(rows, config.Tree{})
	if err != nil || idx != 0 {
		t.Fatalf("nothing active %d %v", idx, err)
	}
	writeHomeEnv(t, s.home(), map[string]string{"SECOND_KEY": "x"})
	if idx, err = s.detectActiveProviderIndex(rows, config.Tree{}); err != nil || idx != 1 {
		t.Fatalf("keyed fallback %d %v", idx, err)
	}
	if idx, err = s.detectActiveProviderIndex(rows, config.Tree{"tts": map[string]any{"provider": "edge"}}); err != nil || idx != 0 {
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
	write := func(row *jsonutil.Object, cfg config.Tree) config.Tree {
		t.Helper()
		if err := s.writeProviderConfig(row, cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := write(toolRow("Edge", "", "", nil, "tts_provider", "edge"), config.Tree{"tts": map[string]any{}})
	if !anyutil.Equal(cfg["tts"], map[string]any{"provider": "edge"}) {
		t.Fatalf("tts %v", cfg["tts"])
	}
	cfg = write(toolRow("Local", "", "", nil, "browser_provider", "", "browser_engine", "lightpanda"), config.Tree{"browser": map[string]any{}})
	if !anyutil.Equal(cfg["browser"], map[string]any{"engine": "lightpanda"}) {
		t.Fatalf("browser %v", cfg["browser"])
	}
	if err := s.writeProviderConfig(toolRow("L", "", "", nil, "browser_engine", "auto"), config.Tree{"browser": "x"}); err == nil ||
		err.Error() != "browser in config is text, expected a mapping" {
		t.Fatalf("item assignment on str %v", err)
	}
	cfg = write(toolRow("Exa paid", "", "", nil, "web_backend", "exa", "web_tier", "paid"),
		config.Tree{"web": map[string]any{"search_backend": "tavily"}})
	if !anyutil.Equal(cfg["web"], map[string]any{"backend": "exa", "search_backend": "tavily", "provider_tier": map[string]any{"exa": "paid"}}) {
		t.Fatalf("tiered web %v", cfg["web"])
	}
	cfg = write(toolRow("Exa", "", "", nil, "web_backend", "exa"), cfg)
	if !anyutil.Equal(cfg["web"], map[string]any{"backend": "exa", "search_backend": "tavily", "provider_tier": map[string]any{}}) {
		t.Fatalf("untiered web %v", cfg["web"])
	}
	cfg = write(toolRow("cua", "", "", nil, "computer_use_backend", "cua"), config.Tree{})
	if !anyutil.Equal(cfg["computer_use"], map[string]any{"backend": "cua"}) {
		t.Fatalf("computer use %v", cfg["computer_use"])
	}
}
