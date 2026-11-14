package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The hermes _FakeProvider of the image and video picker tests, a titled schema with one key and one model
func pickerFake(name, modelSuffix string, available bool, schema map[string]any) *fakePickerProvider {
	if schema == nil {
		schema = map[string]any{"name": pystr.Title(name), "badge": "test", "tag": name + " test tag",
			"env_vars": []any{map[string]any{"key": strings.ToUpper(name) + "_API_KEY", "prompt": name + " key"}}}
	}
	id := name + modelSuffix
	return &fakePickerProvider{name: name, display: pystr.Title(name), schema: schema, available: available,
		models: []map[string]any{{"id": id, "display": name + " v1", "speed": "~5s", "strengths": "test", "price": "$"}}, def: id}
}

// The hermes _FakeTTSProvider, a catalog provider whose default schema names its title cased display name
func ttsFake(name string) *fakePickerProvider {
	display := pystr.Title(name)
	return &fakePickerProvider{name: name, display: display, available: true,
		schema: map[string]any{"name": display, "badge": "", "tag": "", "env_vars": []any{}}}
}

// Built in TTS provider names of the real registry the fake registry stands in for
func ttsBuiltinNames(t *testing.T) map[string]bool {
	t.Helper()
	if ToolsConfigHooks.TTSRegistry == nil || ToolsConfigHooks.TTSRegistry.BuiltinNames == nil {
		t.Fatal("the media TTS registry is not installed")
	}
	return ToolsConfigHooks.TTSRegistry.BuiltinNames
}

func rowsWith(rows []*pyjson.Object, key string) []*pyjson.Object {
	var out []*pyjson.Object
	for _, r := range rows {
		if pyTruthy(rowGet(r, key)) {
			out = append(out, r)
		}
	}
	return out
}

func rowNames(rows []*pyjson.Object) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = rowText(r, "name")
	}
	return out
}

func TestPluginPickerInjectionVisibleProvidersIncludesPluginsForImageGen(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, pickerFake("someimg", "-model-v1", true, nil)))
	rows, err := s.VisibleProviders(categoryOf("image_gen"), config.Tree{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var plugins []string
	for _, r := range rowsWith(rows, "image_gen_plugin_name") {
		plugins = append(plugins, rowText(r, "image_gen_plugin_name"))
	}
	if !slices.Contains(plugins, "someimg") {
		t.Fatalf("plugin rows %v", plugins)
	}
}

func TestPluginCatalogPluginCatalogReturnsModels(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, pickerFake("catimg", "-model-v1", true, nil)))
	catalog, def := s.pluginImageGenCatalog("catimg")
	if !catalog.has("catimg-model-v1") || def != "catimg-model-v1" {
		t.Fatalf("catalog %v default %q", catalog.ids, def)
	}
}

func TestConfigPromptImageGenSatisfiedByPluginProvider(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "FAL_KEY")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, pickerFake("avail-img", "-model-v1", true, nil)))
	if needs, err := s.toolsetNeedsConfigurationPrompt("image_gen", config.Tree{}, false); err != nil || needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestConfigWritingPickingPluginProviderWritesProviderAndModel(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil,
		pickerFake("noenv", "-model-v1", true, map[string]any{"name": "NoEnv", "badge": "free", "tag": "", "env_vars": []any{}})))
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 0, nil })
	cfg := config.Tree{}
	row := caseRow("name", "NoEnv", "env_vars", []any{}, "image_gen_plugin_name", "noenv")
	if err := s.configureProvider(row, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	imageGen := section(cfg, "image_gen")
	if imageGen["provider"] != "noenv" || imageGen["model"] != "noenv-model-v1" {
		t.Fatalf("image_gen %v", imageGen)
	}
}

func TestPluginTTSProvidersSkipsProvidersWithNoName(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(nil,
		&fakePickerProvider{display: "Bogus", schema: map[string]any{"name": "Bogus"}, available: true}))
	for _, r := range s.pluginTTSProviders() {
		if rowText(r, "tts_plugin_name") == "bogus" {
			t.Fatalf("nameless provider listed %v", rowNames(s.pluginTTSProviders()))
		}
	}
}

func TestPluginTTSProvidersMinimalSchemaUsesDisplayName(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(ttsBuiltinNames(t), ttsFake("minimal")))
	rows := s.pluginTTSProviders()
	if len(rows) != 1 {
		t.Fatalf("rows %v", rowNames(rows))
	}
	envVars, _ := rowGet(rows[0], "env_vars").([]any)
	if rowText(rows[0], "name") != "Minimal" || rowText(rows[0], "tts_provider") != "minimal" || envVars == nil || len(envVars) != 0 {
		t.Fatalf("row %v", rowPairs(rows[0]))
	}
}

func TestVisibleProvidersInjectsTTSPluginsTtsCategoryIncludesPluginRows(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(ttsBuiltinNames(t), ttsFake("cartesia")))
	rows, err := s.VisibleProviders(categoryOf("tts"), config.Tree{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rowNames(rows), "Cartesia") {
		t.Fatalf("names %v", rowNames(rows))
	}
	plugin := rowsWith(rows, "tts_plugin_name")
	if len(plugin) != 1 || rowText(plugin[0], "tts_provider") != "cartesia" {
		t.Fatalf("plugin rows %v", rowNames(plugin))
	}
}

func TestVisibleProvidersInjectsTTSPluginsOtherCategoriesUnaffectedByTtsPlugins(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.TTSRegistry, fakePickerRegistry(ttsBuiltinNames(t), ttsFake("cartesia")))
	rows, err := s.VisibleProviders(categoryOf("image_gen"), config.Tree{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rowNames(rows), "Cartesia") {
		t.Fatalf("names %v", rowNames(rows))
	}
}

func TestReconfigureWritesProviderReconfigureWithEnvVarsAlreadySetWritesProvider(t *testing.T) {
	s, _ := newTestToolsSetup(t, "\n\n\n")
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, pickerFake("xai_fake", "-video-v1", true, nil)))
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 0, nil })
	swapHook(t, &ToolsConfigHooks.SetupPrompt, func(string, string, bool) string { return "" })
	s.Con.MaskedSecretPrompt = func(string) (string, error) { return "", nil }
	t.Setenv("XAI_FAKE_API_KEY", "sk-fake")
	cfg := config.Tree{}
	row := caseRow("name", "xAI", "env_vars", []any{caseRow("key", "XAI_FAKE_API_KEY", "prompt", "xAI key")}, "video_gen_plugin_name", "xai_fake")
	if err := s.reconfigureProvider(row, cfg, true); err != nil {
		t.Fatal(err)
	}
	videoGen := section(cfg, "video_gen")
	if _, has := videoGen["use_gateway"]; videoGen["provider"] != "xai_fake" || videoGen["model"] != "xai_fake-video-v1" || has {
		t.Fatalf("video_gen %v", videoGen)
	}
}

func TestPluginVideoProvidersRowPostSetupPropagatedWhenDeclared(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, pickerFake("xai_video", "-video-v1", true,
		map[string]any{"name": "xAI Grok Imagine", "badge": "paid", "tag": "grok video", "env_vars": []any{}, "post_setup": "xai_grok"})))
	for _, r := range s.pluginVideoGenProviders() {
		if rowText(r, "video_gen_plugin_name") == "xai_video" {
			if rowText(r, "post_setup") != "xai_grok" {
				t.Fatalf("row %v", rowPairs(r))
			}
			return
		}
	}
	t.Fatal("xai_video row missing")
}

func TestPoolOnlyAccountIsOfferedFalModelsOnly(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, map[string]NousToolFeature{}, accountDouble(true, boolRef(false), true, "fal")))
	catalog, _ := s.managedImageCatalog(config.Tree{})
	if len(catalog.ids) == 0 {
		t.Fatal("empty catalog")
	}
	for _, id := range catalog.ids {
		if backend := catalog.meta[id]["backend"]; backend != "fal" {
			t.Fatalf("%s backend %v", id, backend)
		}
	}
}

func TestVisionPickerCustomEndpoint(t *testing.T) {
	s, _ := newTestToolsSetup(t, "https://my.endpoint/v1\nmy-vision-model\n")
	s.Con.MaskedSecretPrompt = func(string) (string, error) { return "sk-secret", nil }
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 2, nil })
	swapHook(t, &toolsetHasKeysFn, func(*ToolsSetup, string, config.Tree, bool, *NousToolFeatures) (bool, error) { return false, nil })
	unsetEnv(t, "OPENAI_API_KEY")
	if err := s.configureVisionBackend(); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	vision, _ := pyDictOf(section(cfg, "auxiliary")["vision"])
	if vision["base_url"] != "https://my.endpoint/v1" || vision["model"] != "my-vision-model" || vision["provider"] != "custom" {
		t.Fatalf("vision %v", vision)
	}
	env, err := os.ReadFile(filepath.Join(s.home(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, line := range strings.Split(string(env), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.TrimSpace(k), "#") {
			keys = append(keys, k+"="+v)
		}
	}
	if !slices.Equal(keys, []string{"OPENAI_API_KEY=sk-secret"}) {
		t.Fatalf("env %v", keys)
	}
}

func TestResolveToolsetModelPluginBranches(t *testing.T) {
	for _, c := range []struct {
		ts   string
		row  *pyjson.Object
		want string
	}{
		{"image_gen", caseRow("image_gen_plugin_name", "openai", "imagegen_backend", "fal"), "openai"},
		{"image_gen", caseRow("image_gen_plugin_name", "", "imagegen_backend", "nous"), "nous"},
		{"image_gen", caseRow("name", "x"), ""},
		{"video_gen", caseRow("video_gen_plugin_name", "xai", "image_gen_plugin_name", "fal"), "xai"},
		{"tts", caseRow("image_gen_plugin_name", "fal"), ""},
	} {
		if got := resolveToolsetModelPlugin(c.ts, c.row); got != c.want {
			t.Fatalf("%s %v: %q", c.ts, rowPairs(c.row), got)
		}
	}
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, pickerFake("catimg", "-model-v1", true, nil)))
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, pickerFake("vidx", "-video-v1", true, nil)))
	if c, def := s.toolsetModelCatalog("image_gen", "catimg", config.Tree{}); def != "catimg-model-v1" || !c.has(def) {
		t.Fatalf("plugin image catalog %v %q", c.ids, def)
	}
	falCat, falDef := imagegenBackends["fal"].catalogFn(s, config.Tree{})
	if c, def := s.toolsetModelCatalog("image_gen", "fal", config.Tree{}); def != falDef || !slices.Equal(c.ids, falCat.ids) {
		t.Fatalf("fal catalog %q", def)
	}
	if c, def := s.toolsetModelCatalog("video_gen", "vidx", config.Tree{}); def != "vidx-video-v1" || !c.has(def) {
		t.Fatalf("video catalog %v %q", c.ids, def)
	}
}
