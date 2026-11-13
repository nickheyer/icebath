package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"gopkg.in/yaml.v3"
)

// Masked prompt double answering from a queue and recording each prompt it showed
func secretAnswers(s *ToolsSetup, answers ...string) *[]string {
	shown := &[]string{}
	s.Con.MaskedSecretPrompt = func(prompt string) (string, error) {
		*shown = append(*shown, prompt)
		if len(answers) == 0 {
			return "", cliui.ErrEOF
		}
		next := answers[0]
		answers = answers[1:]
		return next, nil
	}
	return shown
}

// Asserts every line appears in the output in order
func assertLinesInOrder(t *testing.T, out string, lines ...string) {
	t.Helper()
	rest := out
	for _, line := range lines {
		i := strings.Index(rest, line)
		if i < 0 {
			t.Fatalf("missing %q after earlier lines in output\n%s", line, out)
		}
		rest = rest[i+len(line):]
	}
}

func TestConfigureToolCategoryMenuAndSkip(t *testing.T) {
	unsetEnv(t, "XAI_API_KEY")
	s, out := newTestToolsSetup(t, "3\n")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, nil, nil))
	cat := catalogCategory("x_search")
	cfg := config.Tree{}
	if err := s.configureToolCategory("x_search", cat, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	note := rowText(cat, "setup_note")
	assertLinesInOrder(t, out.String(),
		"\n  --- 🐦 X (Twitter) Search - Select xAI Credential Source ---\n",
		"    "+note+"\n\n",
		"  Select xAI Credential Source:",
		" 1. xAI Grok OAuth (SuperGrok / Premium+) [subscription] — Browser login at accounts.x.ai — no API key required\n",
		" 2. xAI API key [paid] — Direct xAI API billing via XAI_API_KEY\n",
		" 3. Skip — keep defaults / configure later\n",
		"    Skipped X (Twitter) Search\n")
	if len(cfg) != 0 {
		t.Fatalf("skip wrote %v", cfg)
	}
	s, out = newTestToolsSetup(t, "2\n")
	shown := secretAnswers(s, "xai-typed")
	if err := s.configureToolCategory("x_search", cat, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.getEnvValue("XAI_API_KEY"); got != "xai-typed" {
		t.Fatalf("saved key %q", got)
	}
	os.Unsetenv("XAI_API_KEY")
	if !slices.Equal(*shown, []string{"      xAI API key: "}) {
		t.Fatalf("secret prompts %q", *shown)
	}
	assertLinesInOrder(t, out.String(), "    Get yours at: https://console.x.ai/\n", "✓     Saved\n", "✓   xAI API key configured!\n")
	if len(cfg) != 0 {
		t.Fatalf("config %v", cfg)
	}
}

func TestConfigureToolCategoryMarksRowsAndReconfigures(t *testing.T) {
	s, out := newTestToolsSetup(t, "4\n")
	swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { return nil })
	writeHomeEnv(t, s.home(), map[string]string{"GROQ_API_KEY": "gsk", "VOICE_TOOLS_OPENAI_KEY": "sk"})
	unsetEnv(t, "GROQ_API_KEY", "VOICE_TOOLS_OPENAI_KEY")
	installSTTCatalogDoubles(t)
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, map[string]NousToolFeature{"stt": {}}, nil))
	cfg := config.Tree{"stt": map[string]any{"provider": "groq"}}
	if err := s.configureToolCategory("stt", catalogCategory("stt"), cfg, true, false); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(),
		"  --- 🎙️ Speech-to-Text - Choose a provider ---\n",
		" 1. Local Whisper [★ recommended · free] — faster-whisper on-device, no API key\n",
		" 2. Nous Subscription [subscription] — Managed OpenAI transcription billed to your subscription  ★ Included with your Nous subscription\n",
		" 3. OpenAI [paid] — whisper-1, gpt-4o-transcribe, gpt-transcribe [configured]\n",
		" 4. Groq [free tier] — whisper-large-v3-turbo, whisper-large-v3 — very fast [active]\n",
		"Choice [default 3]: ",
		"✓   STT provider set to: groq\n",
		"✓   GROQ_API_KEY: already configured\n",
		"✓   Groq configured!\n",
		"✓   STT model set to: whisper-large-v3-turbo\n")
	if v, _ := config.CfgGet(cfg, "stt", "groq", "model"); v != "whisper-large-v3-turbo" {
		t.Fatalf("stt model %v", v)
	}
	s, out = newTestToolsSetup(t, "1\n\n\n")
	swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { return nil })
	writeHomeEnv(t, s.home(), map[string]string{"GROQ_API_KEY": "gsk-123456789"})
	if err := s.configureToolCategory("stt", catalogCategory("stt"), cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Skip — keep defaults") || strings.Contains(out.String(), "★ Included") {
		t.Fatalf("reconfigure menu kept new enable rows\n%s", out.String())
	}
	assertLinesInOrder(t, out.String(), "  --- 🎙️ Speech-to-Text - Choose a provider ---\n", "  Select provider:", "✓   STT provider set to: local\n")
}

func TestConfigureToolCategorySingleProviderRunsItsHook(t *testing.T) {
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ComputerUseDefaultBackend, func() string { return "cua" })
	swapHook(t, &ToolsConfigHooks.ComputerUseProviders, func(*Env) ([][2]string, error) { return [][2]string{{"cua", "x"}}, nil })
	installs := 0
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(context.Context, *Env, *cliui.Console) (bool, error) { installs++; return true, nil })
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, nil, nil))
	cfg := config.Tree{}
	if err := s.configureToolCategory("computer_use", catalogCategory("computer_use"), cfg, true, false); err != nil {
		t.Fatal(err)
	}
	want := "\n  --- 🖱️ Computer Use (macOS/Windows/Linux) (cua-driver (background)) ---\n" +
		"    Background computer-use via cua-driver — does NOT steal your cursor or focus. Works with any model.\n" +
		"✓   cua-driver (background) - no configuration needed!\n"
	if out.String() != want || installs != 1 || !pyjson.Equal(map[string]any(cfg), map[string]any{"computer_use": map[string]any{"backend": "cua"}}) {
		t.Fatalf("output %q installs %d cfg %v", out.String(), installs, cfg)
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(context.Context, *Env, *cliui.Console) (bool, error) { installs++; return true, nil })
	if err := s.configureToolCategory("computer_use", catalogCategory("computer_use"), cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\n  --- 🖱️ Computer Use (macOS/Windows/Linux) (cua-driver (background)) ---\n"+
		"✓   Computer Use backend set to: cua\n✓   cua-driver (background) - no configuration needed!\n" {
		t.Fatalf("reconfigure output %q", out.String())
	}
}

func TestPickModelFromCatalogListsTheCurrentModelFirst(t *testing.T) {
	s, out := newTestToolsSetup(t, "2\n")
	cat := catalogOf([]string{"fal-ai/a", "fal-ai/longer-name"}, map[string]map[string]any{
		"fal-ai/a":           {"speed": "<1s", "strengths": "Fast", "price": "$0.01"},
		"fal-ai/longer-name": {"speed": "~10s", "strengths": "Photoreal detail", "price": 3},
	})
	cfg := config.Tree{"image_gen": map[string]any{"model": "fal-ai/longer-name"}}
	if err := s.pickModelFromCatalog(cat, "fal-ai/a", "image_gen", "FAL.ai", cfg, "  "); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(),
		"\n  Model"+strings.Repeat(" ", 15)+"Speed  Strengths"+strings.Repeat(" ", 9)+"Price\n",
		"  Choose FAL.ai model:",
		" 1.   fal-ai/longer-name  ~10s  Photoreal detail  3  ← currently in use\n",
		" 2.   fal-ai/a"+strings.Repeat(" ", 12)+"<1s   Fast"+strings.Repeat(" ", 14)+"$0.01\n",
		"✓   Model set to: fal-ai/a\n")
	if v, _ := config.CfgGet(cfg, "image_gen", "model"); v != "fal-ai/a" {
		t.Fatalf("model %v", v)
	}
	for _, c := range []struct {
		current any
		def     string
		first   string
	}{{nil, "fal-ai/longer-name", "fal-ai/longer-name"}, {"retired-model", "", "fal-ai/a"}, {"retired-model", "gone", "fal-ai/a"}, {7, "fal-ai/longer-name", "fal-ai/longer-name"}} {
		s, out := newTestToolsSetup(t, "\n")
		cfg := config.Tree{"image_gen": map[string]any{"model": c.current}}
		if err := s.pickModelFromCatalog(cat, c.def, "image_gen", "FAL.ai", cfg, ""); err != nil {
			t.Fatal(err)
		}
		if v, _ := config.CfgGet(cfg, "image_gen", "model"); v != c.first || !strings.Contains(out.String(), " 1. "+c.first) {
			t.Fatalf("%+v chose %v\n%s", c, v, out.String())
		}
	}
	s, _ = newTestToolsSetup(t, "")
	if err := s.pickModelFromCatalog(cat, "", "image_gen", "FAL.ai", config.Tree{"image_gen": map[string]any{"model": []any{"x"}}}, ""); err == nil {
		t.Fatal("unhashable model accepted")
	}
	bad := catalogOf([]string{"m"}, map[string]map[string]any{"m": {"speed": 4}})
	if err := s.pickModelFromCatalog(bad, "", "image_gen", "X", config.Tree{}, ""); err == nil || err.Error() != "TypeError: object of type 'int' has no len()" {
		t.Fatalf("non string speed %v", err)
	}
	if err := s.pickModelFromCatalog(newModelCatalog(), "", "image_gen", "X", config.Tree{}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureImagegenModelAndPluginPickers(t *testing.T) {
	s, out := newTestToolsSetup(t, "\n")
	swapHook(t, &ToolsConfigHooks.FALModels, func() ([]string, map[string]map[string]any, string) {
		return []string{"fal-ai/flux"}, map[string]map[string]any{"fal-ai/flux": {"speed": "1s"}}, "fal-ai/flux"
	})
	cfg := config.Tree{}
	if err := s.configureImagegenModel("fal", cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.configureImagegenModel("unknown", cfg); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(), "  Choose FAL.ai model:", "✓   Model set to: fal-ai/flux\n")
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "veo", def: "veo-3",
		models: []map[string]any{{"id": "veo-3", "speed": "60s"}}}))
	s, out = newTestToolsSetup(t, "\n")
	if err := s.configureVideogenModelForPlugin("veo", cfg); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(), "  Choose veo model:", " 1.   veo-3  60s      ← currently in use\n")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "fluxy", def: "f1",
		models: []map[string]any{{"id": "f1"}}}))
	s, out = newTestToolsSetup(t, "\n")
	if err := s.configureImagegenModelForPlugin("fluxy", cfg); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(), " 1. f1        ← currently in use\n")
}

func TestConfigureXAIImagineStorageChoices(t *testing.T) {
	for _, c := range []struct {
		input   string
		storage map[string]any
		success string
	}{
		{"\n", map[string]any{"enabled": true, "public_url": true, "expires_after": nil}, "✓   xAI stored public URLs enabled without automatic expiry\n"},
		{"2\n", map[string]any{"enabled": false}, "✓   xAI stored public URLs disabled\n"},
		{"3\n", map[string]any{"enabled": true, "public_url": true, "expires_after": 172800}, "✓   xAI stored public URLs enabled for 2 days\n"},
	} {
		s, out := newTestToolsSetup(t, c.input)
		cfg := config.Tree{"video_gen": "corrupt"}
		if err := s.configureXAIImagineStorage("video_gen", cfg); err != nil {
			t.Fatal(err)
		}
		if got, _ := config.CfgGet(cfg, "video_gen", "xai", "storage"); !pyjson.Equal(got, c.storage) {
			t.Fatalf("input %q storage %v", c.input, got)
		}
		assertLinesInOrder(t, out.String(),
			"⚠   xAI Imagine can store generated media and create reusable public URLs. xAI may bill for stored files and public URL hosting.\n",
			"  Stored public URLs:", " 1. Enable public URLs without automatic expiry (recommended)\n", " 2. Disable stored public URLs\n",
			" 3. Enable public URLs for 2 days\n", c.success)
	}
}

func TestSelectPluginGenProviderWritesTheVendorOrNous(t *testing.T) {
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "xai"}))
	s, out := newTestToolsSetup(t, "2\n")
	cfg := config.Tree{"video_gen": map[string]any{"use_gateway": true}}
	if err := s.selectPluginVideoGenProvider("xai", cfg, false); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["video_gen"], map[string]any{"provider": "xai", "xai": map[string]any{"storage": map[string]any{"enabled": false}}}) {
		t.Fatalf("video section %v", cfg["video_gen"])
	}
	assertLinesInOrder(t, out.String(), "✓   video_gen.provider set to: xai\n", "✓   xAI stored public URLs disabled\n")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "fluxy"}))
	s, out = newTestToolsSetup(t, "")
	if err := s.selectPluginImageGenProvider("fluxy", cfg, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "✓   image_gen.provider set to: nous\n" || !pyjson.Equal(cfg["image_gen"], map[string]any{"provider": "nous"}) {
		t.Fatalf("managed pick %q %v", out.String(), cfg["image_gen"])
	}
}

func TestConfigureSTTModelCatalogs(t *testing.T) {
	installSTTCatalogDoubles(t)
	s, out := newTestToolsSetup(t, "\n")
	cfg := config.Tree{"stt": map[string]any{"elevenlabs": map[string]any{"model_id": " scribe_v1 "}}}
	if err := s.configureSTTModel("elevenlabs", cfg); err != nil {
		t.Fatal(err)
	}
	if v, _ := config.CfgGet(cfg, "stt", "elevenlabs", "model_id"); v != "scribe_v1" {
		t.Fatalf("elevenlabs model %v", v)
	}
	assertLinesInOrder(t, out.String(), "  Select STT model:", "Choice [default 2]: ", "✓   STT model set to: scribe_v1\n")
	var tags []string
	swapHook(t, &ToolsConfigHooks.DeepInfraModelIDs, func(_ context.Context, _ *Env, tag string) []string {
		tags = append(tags, tag)
		return []string{"openai/whisper-large-v3", "distil-whisper"}
	})
	s, out = newTestToolsSetup(t, "2\n")
	if err := s.configureSTTModel("deepinfra", cfg); err != nil {
		t.Fatal(err)
	}
	if v, _ := config.CfgGet(cfg, "stt", "deepinfra", "model"); v != "distil-whisper" || !slices.Equal(tags, []string{"stt"}) {
		t.Fatalf("deepinfra model %v tags %v", v, tags)
	}
	swapHook(t, &ToolsConfigHooks.DeepInfraModelIDs, func(context.Context, *Env, string) []string { return nil })
	s, out = newTestToolsSetup(t, "")
	if err := s.configureSTTModel("deepinfra", config.Tree{}); err != nil || out.Len() != 0 {
		t.Fatalf("empty live catalog %v %q", err, out.String())
	}
	if err := s.configureSTTModel("unknown", config.Tree{}); err != nil || out.Len() != 0 {
		t.Fatalf("unknown provider %v %q", err, out.String())
	}
}

func TestFinishProviderSelectionRunsTheModelPickers(t *testing.T) {
	installSTTCatalogDoubles(t)
	swapHook(t, &ToolsConfigHooks.FALModels, func() ([]string, map[string]map[string]any, string) {
		return []string{"fal-ai/flux"}, map[string]map[string]any{"fal-ai/flux": {}}, "fal-ai/flux"
	})
	s, _ := newTestToolsSetup(t, "\n")
	cfg := config.Tree{"image_gen": map[string]any{"use_gateway": true}}
	if err := s.finishProviderSelection(toolRow("FAL", "", "", nil, "imagegen_backend", "fal"), cfg, ""); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["image_gen"], map[string]any{"model": "fal-ai/flux", "provider": "fal"}) {
		t.Fatalf("fal pick %v", cfg["image_gen"])
	}
	swapHook(t, &ToolsConfigHooks.ManagedImageCatalog, func(context.Context, *Env, bool, bool) ([]string, map[string]map[string]any, string) {
		return []string{"krea-3"}, map[string]map[string]any{"krea-3": {}}, "krea-3"
	})
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, nil, nil))
	s, _ = newTestToolsSetup(t, "\n")
	if err := s.finishProviderSelection(toolRow("Nous", "", "", nil, "imagegen_backend", "nous", "managed_nous_feature", "image_gen"), cfg, "image_gen"); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(cfg["image_gen"], map[string]any{"model": "krea-3", "provider": "nous"}) {
		t.Fatalf("managed pick %v", cfg["image_gen"])
	}
	s, out := newTestToolsSetup(t, "")
	if err := s.finishProviderSelection(toolRow("Nous STT", "", "", nil, "stt_provider", "openai", "managed_nous_feature", "stt"), cfg, "stt"); err != nil || out.Len() != 0 {
		t.Fatalf("managed stt prompted %v %q", err, out.String())
	}
}

func TestNousProviderGateBranches(t *testing.T) {
	s, out := newTestToolsSetup(t, "")
	var asked [][2]string
	swapHook(t, &ToolsConfigHooks.EnsureNousPortalAccess, func(_ context.Context, _ string, w io.Writer, capability, category string) bool {
		asked = append(asked, [2]string{capability, category})
		return len(asked) > 1
	})
	swapHook(t, &ToolsConfigHooks.ManagedCoverageCategory, func(f string) (string, bool) { return "fal-image", f == "image_gen" })
	managed := toolRow("Nous Subscription", "", "", nil, "managed_nous_feature", "image_gen")
	if s.nousProviderGate(managed, config.Tree{}, "image_gen", true) {
		t.Fatal("denied portal access passed the gate")
	}
	if !s.nousProviderGate(managed, config.Tree{}, "image_gen", true) {
		t.Fatal("granted portal access blocked")
	}
	if !slices.Equal(asked, [][2]string{{"Nous Subscription", "fal-image"}, {"Nous Subscription", "fal-image"}}) ||
		out.String() != "⚠   Not enabled — Nous Portal access is required for this backend.\n" {
		t.Fatalf("asked %v output %q", asked, out.String())
	}
	preAuth := toolRow("Nous only", "", "", nil, "requires_nous_auth", true)
	var messages []string
	swapHook(t, &ToolsConfigHooks.NousEntitlementMessage, func(acct *NousToolAccount, capability string) string {
		messages = append(messages, capability)
		if acct != nil {
			return "Upgrade at portal"
		}
		return ""
	})
	for _, c := range []struct {
		f    NousToolFeatures
		pass bool
		want string
	}{
		{NousToolFeatures{}, false, "⚠   Nous Subscription is only available after logging into Nous Portal.\n"},
		{NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, boolRef(false), true)}, false, "⚠   Upgrade at portal\n"},
		{NousToolFeatures{NousAuthPresent: true, Account: accountDouble(true, boolRef(true), true)}, true, ""},
	} {
		s, out := newTestToolsSetup(t, "")
		f := c.f
		swapHook(t, &ToolsConfigHooks.NousFeatures, func(context.Context, string, map[string]any, bool) NousToolFeatures { return f })
		if got := s.nousProviderGate(preAuth, config.Tree{}, "", false); got != c.pass || out.String() != c.want {
			t.Fatalf("%+v got %v %q", c.f, got, out.String())
		}
	}
	if !slices.Equal(messages, []string{"Nous only", "Nous only"}) {
		t.Fatalf("entitlement capabilities %v", messages)
	}
	if !s.nousProviderGate(toolRow("Plain", "", "", nil), config.Tree{}, "", false) {
		t.Fatal("plain row blocked")
	}
}

func TestPrintProviderSelectionLines(t *testing.T) {
	for _, c := range []struct {
		row         *pyjson.Object
		managed     string
		reconfigure bool
		want        string
	}{
		{toolRow("Edge", "", "", nil, "tts_provider", "edge"), "", false, ""},
		{toolRow("Edge", "", "", nil, "tts_provider", "edge"), "", true, "✓   TTS provider set to: edge\n"},
		{toolRow("Local", "", "", nil, "browser_provider", "local", "browser_engine", "auto"), "", false, "✓   Browser set to local mode\n"},
		{toolRow("LP", "", "", nil, "browser_provider", "local", "browser_engine", "lightpanda"), "", false,
			"✓   Browser set to local mode\n✓   Browser engine set to: lightpanda\n"},
		{toolRow("BB", "", "", nil, "browser_provider", "browserbase"), "", false, "✓   Browser cloud provider set to: browserbase\n"},
		{toolRow("Nous BU", "", "", nil, "browser_provider", "", "managed_nous_feature", "browser"), "browser", true, "✓   Browser cloud provider set to: nous\n"},
		{toolRow("Empty", "", "", nil, "browser_provider", ""), "", false, ""},
		{toolRow("Exa", "", "", nil, "web_backend", "exa", "web_tier", "paid"), "", true, "✓   Web backend set to: exa (paid tier)\n"},
		{toolRow("Exa", "", "", nil, "web_backend", "exa", "web_tier", "paid"), "", false, "✓   Web backend set to: exa\n"},
		{toolRow("cua", "", "", nil, "computer_use_backend", "cua"), "", false, ""},
	} {
		s, out := newTestToolsSetup(t, "")
		s.printProviderSelection(c.row, c.managed, c.reconfigure)
		if out.String() != c.want {
			t.Fatalf("%v reconfigure %v output %q", rowPairs(c.row), c.reconfigure, out.String())
		}
	}
}

func TestShowPortalHintForKeyedRowsWithAManagedSibling(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	openaiTTS := catalogProviders(catalogCategory("tts"))[2]
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, nil, nil))
	if !s.showPortalHint(openaiTTS, config.Tree{}, "", false) {
		t.Fatal("no hint for an unauthed keyed row")
	}
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(true, nil, nil))
	if s.showPortalHint(openaiTTS, config.Tree{}, "", false) {
		t.Fatal("hint shown to a Nous user")
	}
	xaiKey := catalogProviders(catalogCategory("x_search"))[1]
	if s.showPortalHint(xaiKey, config.Tree{}, "", false) || s.showPortalHint(openaiTTS, config.Tree{}, "tts", false) {
		t.Fatal("hint for a row without a managed sibling or a managed pick")
	}
	swapHook(t, &ToolsConfigHooks.NousFeatures, func(context.Context, string, map[string]any, bool) NousToolFeatures { panic("portal down") })
	if s.showPortalHint(openaiTTS, config.Tree{}, "", false) {
		t.Fatal("failed feature fetch showed the hint")
	}
}

func TestPromptSecretModes(t *testing.T) {
	unsetEnv(t, "SECRET_PROBE")
	s, out := newTestToolsSetup(t, "")
	shown := secretAnswers(s, "  typed  ")
	ok, err := s.promptSecret("SECRET_PROBE", "Probe key", "https://probe.example", "", false, "Get key at", true)
	if err != nil || !ok {
		t.Fatalf("new enable %v %v", ok, err)
	}
	if got, _ := s.getEnvValue("SECRET_PROBE"); got != "typed" {
		t.Fatalf("saved %q", got)
	}
	if out.String() != "    Get key at: https://probe.example\n✓     Saved\n" || !slices.Equal(*shown, []string{"      Probe key: "}) {
		t.Fatalf("output %q prompts %q", out.String(), *shown)
	}
	s, out = newTestToolsSetup(t, "")
	secretAnswers(s)
	if ok, err := s.promptSecret("SECRET_PROBE", "Probe key", "", "", false, "Get key at", false); err != nil || ok || out.String() != "\n⚠     Skipped\n" {
		t.Fatalf("skipped %v %v %q", ok, err, out.String())
	}
	s, out = newTestToolsSetup(t, "http://localhost:3002\n")
	if ok, err := s.promptSecret("SECRET_URL", "Instance URL", "", "http://default:1", false, "Get yours at", false); err != nil || !ok {
		t.Fatalf("default prompt %v %v", ok, err)
	}
	if got, _ := s.getEnvValue("SECRET_URL"); got != "http://localhost:3002" {
		t.Fatalf("url %q", got)
	}
	os.Unsetenv("SECRET_URL")
	s, out = newTestToolsSetup(t, "")
	writeHomeEnv(t, s.home(), map[string]string{"SECRET_PROBE": "sk-abcdefghijkl"})
	os.Unsetenv("SECRET_PROBE")
	shown = secretAnswers(s, "")
	if ok, err := s.promptSecret("SECRET_PROBE", "Probe key", "", "", true, "Get yours at", false); err != nil || !ok {
		t.Fatalf("reconfigure keep %v %v", ok, err)
	}
	if out.String() != "    SECRET_PROBE: configured (sk-abcde...)\n      Kept current\n" || !slices.Equal(*shown, []string{"      Probe key (Enter to keep current): "}) {
		t.Fatalf("keep output %q prompts %q", out.String(), *shown)
	}
	s, out = newTestToolsSetup(t, "  new-value \n")
	if ok, err := s.promptSecret("SECRET_PLAIN", "Plain", "", "visible", true, "Get yours at", false); err != nil || !ok || out.String() != "      Plain (Enter to keep current): ✓     Updated\n" {
		t.Fatalf("reconfigure update %v %v %q", ok, err, out.String())
	}
	if got, _ := s.getEnvValue("SECRET_PLAIN"); got != "new-value" {
		t.Fatalf("updated %q", got)
	}
	os.Unsetenv("SECRET_PLAIN")
}

func TestConfigureProviderKeyFlows(t *testing.T) {
	unsetEnv(t, "FIRST_KEY", "SECOND_KEY")
	row := toolRow("Two Keys", "paid", "", []any{toolKey("FIRST_KEY", "First key", "https://first.example"), toolKey("SECOND_KEY", "Second key", "")},
		"tts_provider", "twokeys", "post_setup", "browserbase")
	s, out := newTestToolsSetup(t, "")
	writeHomeEnv(t, s.home(), map[string]string{"FIRST_KEY": "one"})
	secretAnswers(s)
	cfg := config.Tree{}
	if err := s.configureProvider(row, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "✓   FIRST_KEY: already configured\n\n⚠     Skipped\n" {
		t.Fatalf("skipped key output %q", out.String())
	}
	if !pyjson.Equal(map[string]any(cfg), map[string]any{"tts": map[string]any{"provider": "twokeys"}}) {
		t.Fatalf("config %v", cfg)
	}
	s, out = newTestToolsSetup(t, "")
	writeHomeEnv(t, s.home(), map[string]string{"FIRST_KEY": "one-1234567890"})
	secretAnswers(s, "", "two")
	if err := s.configureProvider(row, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	want := "✓   TTS provider set to: twokeys\n" +
		"    FIRST_KEY: configured (one-1234...)\n    Get yours at: https://first.example\n      Kept current\n" +
		"✓     Updated\n" +
		"✓     browser_exec ready (built in)\n"
	if out.String() != want {
		t.Fatalf("reconfigure output %q", out.String())
	}
	if got, _ := s.getEnvValue("SECOND_KEY"); got != "two" {
		t.Fatalf("second key %q", got)
	}
	os.Unsetenv("SECOND_KEY")
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, nil, nil))
	openaiTTS := catalogProviders(catalogCategory("tts"))[2]
	unsetEnv(t, "VOICE_TOOLS_OPENAI_KEY")
	secretAnswers(s, "sk-voice")
	if err := s.reconfigureProvider(openaiTTS, cfg, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Available through Nous Portal") {
		t.Fatal("reconfigure showed the portal hint")
	}
	os.Unsetenv("VOICE_TOOLS_OPENAI_KEY")
	s, out = newTestToolsSetup(t, "")
	secretAnswers(s, "sk-voice")
	if err := s.configureProvider(openaiTTS, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	assertLinesInOrder(t, out.String(), "    Available through Nous Portal subscription.\n", "✓     Saved\n", "✓   OpenAI TTS configured!\n")
	os.Unsetenv("VOICE_TOOLS_OPENAI_KEY")
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &nousProviderGateFn, func(*ToolsSetup, *pyjson.Object, config.Tree, string, bool) bool { return false })
	if err := s.configureProvider(openaiTTS, cfg, true, false); err != nil || out.Len() != 0 {
		t.Fatalf("gated row ran %v %q", err, out.String())
	}
}

func TestConfigureVisionBackendChoices(t *testing.T) {
	s, out := newTestToolsSetup(t, "\n")
	seed := config.Tree{"auxiliary": map[string]any{"vision": map[string]any{"provider": "openrouter", "model": "m", "base_url": "u", "api_key": "k", "api_mode": "chat", "timeout": 30}}}
	if err := s.saveConfig(seed); err != nil {
		t.Fatal(err)
	}
	if err := s.configureVisionBackend(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.home(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	fileVision, _ := config.CfgGet(file, "auxiliary", "vision")
	if fv, _ := fileVision.(map[string]any); fv["timeout"] != 30 || fv["provider"] == "openrouter" || fv["model"] == "m" || fv["base_url"] == "u" ||
		fv["api_key"] == "k" || fv["api_mode"] != nil {
		t.Fatalf("auto vision in config file %v", fileVision)
	}
	cfg, _ := s.loadConfig()
	assertVisionKeys(t, cfg, map[string]any{"timeout": 30, "provider": "auto", "model": "", "base_url": "", "api_key": ""}, "api_mode")
	assertLinesInOrder(t, out.String(), "\n  Vision / Image Analysis needs a multimodal model.\n", "  Pick any provider + model (like /model), or let it auto-detect.\n",
		"  Configure vision backend", " 1. Auto — use your main model / aggregator fallback (recommended)\n", " 2. Pick a provider and model\n",
		" 3. Custom OpenAI-compatible endpoint — base URL, API key, model\n", " 4. Skip\n", "✓   Vision set to auto (main model / aggregator fallback)\n")
	s, out = newTestToolsSetup(t, "4\n")
	if err := s.configureVisionBackend(); err != nil || !strings.HasSuffix(out.String(), "    Skipped vision configuration\n") {
		t.Fatalf("skip %v %q", err, out.String())
	}
	unsetEnv(t, "OPENAI_API_KEY")
	s, out = newTestToolsSetup(t, "3\n\n\n")
	shown := secretAnswers(s, " sk-native ")
	if err := s.configureVisionBackend(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.loadConfig()
	assertVisionKeys(t, cfg, map[string]any{"provider": "custom", "base_url": "https://api.openai.com/v1", "model": "gpt-4o-mini"})
	if got, _ := s.getEnvValue("OPENAI_API_KEY"); got != "sk-native" || !slices.Equal(*shown, []string{"      OPENAI_API_KEY: "}) {
		t.Fatalf("key %q prompts %q", got, *shown)
	}
	assertLinesInOrder(t, out.String(), "    Base URL (blank for OpenAI): ", "    Vision model (blank for gpt-4o-mini): ", "✓   Vision set to custom endpoint (gpt-4o-mini)\n")
	os.Unsetenv("OPENAI_API_KEY")
	s, out = newTestToolsSetup(t, "3\nhttp://localhost:8000/v1\n\n")
	if err := s.saveConfig(config.Tree{"auxiliary": map[string]any{"vision": map[string]any{"model": "old"}}}); err != nil {
		t.Fatal(err)
	}
	shown = secretAnswers(s, "local-key")
	if err := s.configureVisionBackend(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.loadConfig()
	assertVisionKeys(t, cfg, map[string]any{"provider": "custom", "base_url": "http://localhost:8000/v1", "model": ""})
	if !slices.Equal(*shown, []string{"      API key: "}) || !strings.HasSuffix(out.String(), "✓   Vision set to custom endpoint\n") {
		t.Fatalf("prompts %q output %q", *shown, out.String())
	}
	os.Unsetenv("OPENAI_API_KEY")
	s, out = newTestToolsSetup(t, "3\n\n")
	secretAnswers(s, "   ")
	if err := s.configureVisionBackend(); err != nil || !strings.HasSuffix(out.String(), "⚠     Skipped\n") {
		t.Fatalf("blank key %v %q", err, out.String())
	}
	s, _ = newTestToolsSetup(t, "3\nhttp://[::1\n")
	secretAnswers(s, "k")
	if err := s.configureVisionBackend(); err == nil {
		t.Fatal("bad base url accepted")
	}
}

func TestConfigureVisionProviderModelPicker(t *testing.T) {
	rows := []map[string]any{
		{"slug": "openrouter", "name": "OpenRouter", "models": []string{"google/gemini-flash", "openai/gpt-4o"}},
		{"slug": "local", "name": "", "models": []any{"llava"}},
	}
	var asked []string
	swapHook(t, &ToolsConfigHooks.AuxPickerRows, func(_ context.Context, cp, cm, cb string, max int) ([]map[string]any, error) {
		asked = append(asked, cp+"|"+cm+"|"+cb)
		if max != 40 {
			t.Fatalf("max models %d", max)
		}
		return rows, nil
	})
	swapHook(t, &ToolsConfigHooks.AuxPickerLabels, func(r []map[string]any, cp, cb string) []string {
		return []string{"OpenRouter (2 models)", "Local"}
	})
	s, out := newTestToolsSetup(t, "1\n2\n")
	vision := map[string]any{"provider": " openrouter ", "model": "m", "base_url": "http://old", "api_key": "k"}
	cfg := config.Tree{"auxiliary": map[string]any{"vision": vision}}
	if err := s.configureVisionProviderModel(cfg, vision); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(vision, map[string]any{"provider": "openrouter", "model": "openai/gpt-4o"}) || !slices.Equal(asked, []string{"openrouter|m|http://old"}) {
		t.Fatalf("vision %v asked %v", vision, asked)
	}
	assertLinesInOrder(t, out.String(), "  Choose vision provider:", " 1. OpenRouter (2 models)\n", " 2. Local\n", " 3. Cancel\n",
		"  Choose vision model for OpenRouter:", " 3. Type a custom model id…\n", "✓   Vision set to openrouter / openai/gpt-4o\n")
	saved, _ := s.loadConfig()
	if v, _ := config.CfgGet(saved, "auxiliary", "vision", "model"); v != "openai/gpt-4o" {
		t.Fatalf("saved vision %v", v)
	}
	s, out = newTestToolsSetup(t, "2\n2\nmy/llava-13b\n")
	vision = map[string]any{}
	if err := s.configureVisionProviderModel(config.Tree{"auxiliary": map[string]any{"vision": vision}}, vision); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(vision, map[string]any{"provider": "local", "model": "my/llava-13b"}) {
		t.Fatalf("custom model %v", vision)
	}
	assertLinesInOrder(t, out.String(), "  Choose vision model for local:", "    Model id: ")
	s, out = newTestToolsSetup(t, "2\n2\n\n")
	if err := s.configureVisionProviderModel(config.Tree{}, map[string]any{}); err != nil || !strings.HasSuffix(out.String(), "⚠   No model entered — cancelled\n") {
		t.Fatalf("empty custom model %v %q", err, out.String())
	}
	s, out = newTestToolsSetup(t, "3\n")
	if err := s.configureVisionProviderModel(config.Tree{}, map[string]any{}); err != nil || !strings.HasSuffix(out.String(), "    Cancelled\n") {
		t.Fatalf("cancel %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.AuxPickerRows, func(context.Context, string, string, string, int) ([]map[string]any, error) {
		return nil, errors.New("ValueError: bad providers block")
	})
	s, out = newTestToolsSetup(t, "")
	if err := s.configureVisionProviderModel(config.Tree{}, map[string]any{}); err != nil ||
		out.String() != "⚠   Could not detect providers: bad providers block\n⚠   No authenticated providers found. Configure a provider first with `icebath model`, then re-run this.\n" {
		t.Fatalf("detection failure %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.AuxPickerRows, nil)
	s, out = newTestToolsSetup(t, "")
	if err := s.configureVisionProviderModel(config.Tree{}, map[string]any{}); err != nil || out.String() != "⚠   Could not load provider list: provider inventory is not installed\n" {
		t.Fatalf("unwired %v %q", err, out.String())
	}
}

func TestConfigureSimpleRequirementsPrompts(t *testing.T) {
	var checks []string
	swapHook(t, &toolsetHasKeysFn, func(_ *ToolsSetup, key string, cfg config.Tree, forceFresh bool, f *NousToolFeatures) (bool, error) {
		checks = append(checks, key)
		return true, nil
	})
	s, out := newTestToolsSetup(t, "")
	if err := s.configureSimpleRequirements("vision", false); err != nil || out.Len() != 0 || !slices.Equal(checks, []string{"vision"}) {
		t.Fatalf("configured vision %v %q %v", err, out.String(), checks)
	}
	swapHook(t, &toolsetHasKeysFn, func(*ToolsSetup, string, config.Tree, bool, *NousToolFeatures) (bool, error) { return false, nil })
	s, out = newTestToolsSetup(t, "4\n")
	if err := s.configureSimpleRequirements("vision", false); err != nil || !strings.Contains(out.String(), "Skipped vision configuration") {
		t.Fatalf("unconfigured vision %v %q", err, out.String())
	}
	s, out = newTestToolsSetup(t, "4\n")
	if err := s.configureSimpleRequirements("vision", true); err != nil || !strings.Contains(out.String(), "Skipped vision configuration") {
		t.Fatalf("reconfigure vision %v %q", err, out.String())
	}
	saved := ToolsetEnvRequirements
	t.Cleanup(func() { ToolsetEnvRequirements = saved })
	ToolsetEnvRequirements = map[string][][2]string{"terminal": {{"REQ_ONE", "https://one.example"}, {"REQ_TWO", "https://two.example"}}}
	unsetEnv(t, "REQ_ONE", "REQ_TWO")
	s, out = newTestToolsSetup(t, "")
	writeHomeEnv(t, s.home(), map[string]string{"REQ_ONE": "set"})
	secretAnswers(s, " two ")
	if err := s.configureSimpleRequirements("terminal", false); err != nil {
		t.Fatal(err)
	}
	label := toolsetLabel(s.ctx(), "terminal")
	if out.String() != "\n  "+label+" requires configuration:\n    Get key at: https://two.example\n✓     Saved\n" {
		t.Fatalf("new enable output %q", out.String())
	}
	if got, _ := s.getEnvValue("REQ_TWO"); got != "two" {
		t.Fatalf("saved %q", got)
	}
	os.Unsetenv("REQ_TWO")
	s, out = newTestToolsSetup(t, "")
	secretAnswers(s, "", "")
	if err := s.configureSimpleRequirements("terminal", true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\n  "+label+":\n    Get key at: https://one.example\n      Kept current\n    Get key at: https://two.example\n      Kept current\n" {
		t.Fatalf("reconfigure output %q", out.String())
	}
	s, out = newTestToolsSetup(t, "")
	if err := s.configureSimpleRequirements("no_requirements", false); err != nil || out.Len() != 0 {
		t.Fatalf("no requirements %v %q", err, out.String())
	}
}

func TestToolsetNeedsConfigurationPromptWithoutCategory(t *testing.T) {
	var asked []string
	swapHook(t, &toolsetHasKeysFn, func(_ *ToolsSetup, key string, _ config.Tree, forceFresh bool, _ *NousToolFeatures) (bool, error) {
		asked = append(asked, key)
		return key == "vision", nil
	})
	s, _ := newTestToolsSetup(t, "")
	for key, want := range map[string]bool{"vision": false, "terminal": true} {
		if got, err := s.toolsetNeedsConfigurationPrompt(key, config.Tree{}, true); err != nil || got != want {
			t.Fatalf("%s needs %v %v", key, got, err)
		}
	}
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"terminal", "vision"}) {
		t.Fatalf("asked %v", asked)
	}
}

// Asserts auxiliary.vision holds the values and lacks the keys
func assertVisionKeys(t *testing.T, cfg config.Tree, want map[string]any, absent ...string) {
	t.Helper()
	raw, _ := config.CfgGet(cfg, "auxiliary", "vision")
	vision, _ := pyDictOf(raw)
	for k, v := range want {
		if !pyjson.Equal(vision[k], v) {
			t.Fatalf("auxiliary.vision.%s = %v in %v", k, vision[k], vision)
		}
	}
	for _, k := range absent {
		if _, ok := vision[k]; ok {
			t.Fatalf("auxiliary.vision kept %s in %v", k, vision)
		}
	}
}

// Rows of a category as objects
func catalogProviders(cat *pyjson.Object) []*pyjson.Object { return catProviders(cat) }
