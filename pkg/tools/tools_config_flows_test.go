package tools

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pytiktoken"
)

// A configurator run over a temp home with captured output and no input
func flowSetup(t *testing.T) (*ToolsSetup, *bytes.Buffer, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var out bytes.Buffer
	return &ToolsSetup{Ctx: home.WithOverride(context.Background(), h), Con: &cliui.Console{In: strings.NewReader(""), Out: &out}}, &out, h
}

// Swaps a seam for the test duration
func swapFor[T any](t *testing.T, seam *T, fn T) {
	t.Helper()
	prev := *seam
	*seam = fn
	t.Cleanup(func() { *seam = prev })
}

func TestToolsetHasKeysForVisionAcceptsCodexAuth(t *testing.T) {
	s, _, h := flowSetup(t)
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(`{"active_provider":"openai-codex","providers":{"openai-codex":{"tokens":{"access_token": "codex-...oken","refresh_token": "codex-...oken"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	swapFor(t, &resolveVisionProviderClient, func(*ToolsSetup) (bool, error) { return true, nil })
	if has, err := s.toolsetHasKeys("vision", nil, false, nil); err != nil || !has {
		t.Fatalf("vision %v %v", has, err)
	}
}

func TestApiServerPlatformConfigDefaultApiServerIncludesTerminalToolset(t *testing.T) {
	if !Default.PlatformTools(context.Background(), "", config.Tree{}, "api_server", PlatformToolsOptions{IncludeDefaultMCPServers: true})["terminal"] {
		t.Fatal("terminal missing from the api_server toolsets")
	}
}

func TestToolsetHasKeysBranches(t *testing.T) {
	s, _, _ := flowSetup(t)
	swapFor(t, &resolveVisionProviderClient, func(*ToolsSetup) (bool, error) { return true, os.ErrNotExist })
	if has, err := s.toolsetHasKeys("vision", config.Tree{}, false, nil); err != nil || has {
		t.Fatalf("vision resolver error %v %v", has, err)
	}
	managed := &NousToolFeatures{Features: map[string]NousToolFeature{"tts": {ManagedByNous: true}, "web": {Available: true}}}
	for _, key := range []string{"tts", "web"} {
		if has, err := s.toolsetHasKeys(key, config.Tree{}, false, managed); err != nil || !has {
			t.Fatalf("%s managed %v %v", key, has, err)
		}
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	if has, err := s.toolsetHasKeys("memory", config.Tree{}, false, nil); err != nil || !has {
		t.Fatalf("keyless toolset %v %v", has, err)
	}
	loads := 0
	swapFor(t, &toolsConfigLoad, func(*ToolsSetup) (config.Tree, error) { loads++; return config.Tree{}, nil })
	swapFor(t, &resolveVisionProviderClient, func(*ToolsSetup) (bool, error) { return false, nil })
	if has, err := s.toolsetHasKeys("vision", nil, false, nil); err != nil || has || loads != 1 {
		t.Fatalf("nil config loads %d %v %v", loads, has, err)
	}
}

func TestPromptToolsetChecklistLabelsAndTokenStatus(t *testing.T) {
	s, _, _ := flowSetup(t)
	swapFor(t, &toolsetHasKeysFn, func(_ *ToolsSetup, tsKey string, cfg config.Tree, _ bool, _ *NousToolFeatures) (bool, error) {
		if cfg != nil {
			t.Fatalf("checklist passed a config for %s", tsKey)
		}
		return tsKey != "vision" && tsKey != "tts", nil
	})
	key := toolTokenKey{home.Key(s.home()), Default.Generation()}
	toolTokenCacheMu.Lock()
	toolTokenCache[key] = map[string]int{"terminal": 900, "process_manage": 200, "read_file": 40}
	toolTokenCacheMu.Unlock()
	t.Cleanup(func() { toolTokenCacheMu.Lock(); delete(toolTokenCache, key); toolTokenCacheMu.Unlock() })
	var title string
	var items []string
	var pre, cancel map[int]bool
	var status func(map[int]bool) string
	swapFor(t, &toolsChecklist, func(_ *ToolsSetup, ti string, it []string, sel, cr map[int]bool, fn func(map[int]bool) string) (map[int]bool, error) {
		title, items, pre, cancel, status = ti, it, sel, cr, fn
		return map[int]bool{0: true, 3: true}, nil
	})
	got, err := s.promptToolsetChecklistImpl("CLI", map[string]bool{"terminal": true, "discord": true, "stt": true}, "cli", true)
	if err != nil {
		t.Fatal(err)
	}
	var effective []ConfigurableToolset
	for _, ts := range EffectiveConfigurableToolsets(s.ctx()) {
		if ToolsetAllowedForPlatform(ts.Key, "cli") && !ConfigOnlyToolsets[ts.Key] {
			effective = append(effective, ts)
		}
	}
	if title != "Tools for CLI" || len(items) != len(effective) {
		t.Fatalf("title %q items %d of %d", title, len(items), len(effective))
	}
	for i, ts := range effective {
		want := ts.Label + "  (" + ts.Description + ")"
		if ts.Key == "vision" || ts.Key == "tts" {
			want += "  [no API key]"
		}
		if items[i] != want {
			t.Fatalf("row %d %q want %q", i, items[i], want)
		}
		if pre[i] != (ts.Key == "terminal") {
			t.Fatalf("preselected %v", pre)
		}
	}
	if !reflect.DeepEqual(pre, cancel) {
		t.Fatalf("cancel %v pre %v", cancel, pre)
	}
	if !reflect.DeepEqual(got, map[string]bool{effective[0].Key: true, effective[3].Key: true}) {
		t.Fatalf("chosen %v", got)
	}
	terminalIdx := -1
	for i, ts := range effective {
		if ts.Key == "terminal" {
			terminalIdx = i
		}
	}
	if s := status(map[int]bool{terminalIdx: true}); s != "Est. tool context: ~1.1k tokens" {
		t.Fatalf("status %q", s)
	}
	if s := status(map[int]bool{}); s != "Est. tool context: ~0 tokens" {
		t.Fatalf("empty status %q", s)
	}
}

func TestPromptToolsetChecklistWithoutTokensHasNoStatus(t *testing.T) {
	s, _, _ := flowSetup(t)
	swapFor(t, &toolsetHasKeysFn, func(*ToolsSetup, string, config.Tree, bool, *NousToolFeatures) (bool, error) { return true, nil })
	key := toolTokenKey{home.Key(s.home()), Default.Generation()}
	toolTokenCacheMu.Lock()
	toolTokenCache[key] = map[string]int{}
	toolTokenCacheMu.Unlock()
	t.Cleanup(func() { toolTokenCacheMu.Lock(); delete(toolTokenCache, key); toolTokenCacheMu.Unlock() })
	swapFor(t, &toolsChecklist, func(_ *ToolsSetup, _ string, _ []string, _, _ map[int]bool, fn func(map[int]bool) string) (map[int]bool, error) {
		if fn != nil {
			t.Fatal("status line without token counts")
		}
		return map[int]bool{-1: true}, nil
	})
	got, err := s.promptToolsetChecklistImpl("Telegram", map[string]bool{}, "telegram", true)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPlatformToolsetsExplicitlySaved(t *testing.T) {
	for _, c := range []struct {
		value any
		want  bool
	}{{[]any{}, true}, {[]any{"web"}, true}, {`["web", "terminal"]`, true}, {nil, false}, {"bogus", false}} {
		cfg := config.Tree{"platform_toolsets": map[string]any{"slack": c.value}}
		if got := PlatformToolsetsExplicitlySaved(cfg, "slack"); got != c.want {
			t.Fatalf("%v: %v", c.value, got)
		}
	}
	if PlatformToolsetsExplicitlySaved(config.Tree{}, "slack") {
		t.Fatal("absent platform saved")
	}
}

func TestPrintToolsetDiffAndConfigureList(t *testing.T) {
	s, out, _ := flowSetup(t)
	s.printToolsetDiff(map[string]bool{"web": true, "memory": true}, map[string]bool{"file": true}, "    ")
	var configured []string
	swapFor(t, &configureToolset, func(_ *ToolsSetup, tsKey string, _ config.Tree, forceFresh, reconfigure bool) error {
		if !forceFresh || reconfigure {
			t.Fatalf("flags %v %v", forceFresh, reconfigure)
		}
		configured = append(configured, tsKey)
		return nil
	})
	if err := s.configureList(nil, config.Tree{}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.configureList([]string{"web", "tts"}, config.Tree{}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.configureList([]string{"vision"}, config.Tree{}, false); err != nil {
		t.Fatal(err)
	}
	want := "    + 💾 Memory\n    + 🔍 Web Search & Scraping\n    - 📁 File Operations\n" +
		"\n  Configuring 2 selected tool(s):\n    • 🔍 Web Search & Scraping\n    • 🔊 Text-to-Speech\n  You can skip any tool you don't need right now.\n\n" +
		"\n  Configuring 1 tool(s):\n    • 👁️  Vision / Image Analysis\n  You can skip any tool you don't need right now.\n\n"
	if out.String() != want {
		t.Fatalf("output %q", out.String())
	}
	if !reflect.DeepEqual(configured, []string{"web", "tts", "vision"}) {
		t.Fatalf("configured %v", configured)
	}
}

func TestPrintToolsSummaryListsEveryPlatform(t *testing.T) {
	s, out, _ := flowSetup(t)
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"web", "memory"}, "telegram": []any{}}}
	if err := s.printToolsSummary(cfg, []string{"cli", "telegram"}); err != nil {
		t.Fatal(err)
	}
	total := len(EffectiveConfigurableToolsets(s.ctx()))
	cli := Default.PlatformTools(s.ctx(), "", cfg, "cli", PlatformToolsOptions{IncludeDefaultMCPServers: true})
	want := "☤ Tool Summary\n\n  CLI  (" + strconv.Itoa(len(cli)) + "/" + strconv.Itoa(total) + ")\n"
	for _, ts := range sortedSet(cli) {
		want += "    ✓ " + toolsetLabel(s.ctx(), ts) + "\n"
	}
	want += "  Telegram  (0/" + strconv.Itoa(total) + ")\n    (none enabled)\n\n"
	if out.String() != want {
		t.Fatalf("output %q want %q", out.String(), want)
	}
	if err := s.printToolsSummary(cfg, []string{"bogus"}); err == nil || err.Error() != "KeyError: 'bogus'" {
		t.Fatalf("unknown platform %v", err)
	}
}

func TestConfigureSharedMetricsInteractiveSavesOnlyOnChange(t *testing.T) {
	s, _, _ := flowSetup(t)
	saves := 0
	swapFor(t, &toolsConfigSave, func(*ToolsSetup, config.Tree) error { saves++; return nil })
	swapFor(t, &setupTelemetry, func(_ *ToolsSetup, cfg config.Tree) error {
		cfg["telemetry"] = map[string]any{"shared_metrics": map[string]any{"enabled": true, "send": true}}
		return nil
	})
	cfg := config.Tree{}
	if err := s.configureSharedMetricsInteractive(cfg); err != nil || saves != 1 {
		t.Fatalf("saves %d %v", saves, err)
	}
	if err := s.configureSharedMetricsInteractive(cfg); err != nil || saves != 1 {
		t.Fatalf("unchanged answer saved, saves %d %v", saves, err)
	}
}

func TestReconfigureToolNothingConfiguredAndCancel(t *testing.T) {
	s, out, _ := flowSetup(t)
	swapFor(t, &toolsetHasKeysFn, func(*ToolsSetup, string, config.Tree, bool, *NousToolFeatures) (bool, error) { return false, nil })
	cfg := config.Tree{"platform_toolsets": map[string]any{}}
	for _, p := range Platforms {
		cfg["platform_toolsets"].(map[string]any)[p.Key] = []any{}
	}
	if err := s.reconfigureTool(cfg, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "  No configured tools to reconfigure.\n" {
		t.Fatalf("output %q", out.String())
	}
	swapFor(t, &toolsetHasKeysFn, func(_ *ToolsSetup, tsKey string, _ config.Tree, _ bool, _ *NousToolFeatures) (bool, error) {
		return tsKey == "web" || tsKey == "tts", nil
	})
	var choices []string
	var def int
	swapFor(t, &toolsPromptChoice, func(_ *ToolsSetup, q string, c []string, d int) (int, error) {
		if q != "  Which tool would you like to reconfigure?" {
			t.Fatalf("question %q", q)
		}
		choices, def = c, d
		return d, nil
	})
	saves := 0
	swapFor(t, &toolsConfigSave, func(*ToolsSetup, config.Tree) error { saves++; return nil })
	if err := s.reconfigureTool(cfg, true); err != nil || saves != 0 {
		t.Fatalf("cancel saved %d %v", saves, err)
	}
	if !reflect.DeepEqual(choices, []string{"🔍 Web Search & Scraping", "🔊 Text-to-Speech", "Cancel"}) || def != 2 {
		t.Fatalf("choices %v default %d", choices, def)
	}
	var picked []string
	swapFor(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 1, nil })
	swapFor(t, &configureToolset, func(_ *ToolsSetup, tsKey string, _ config.Tree, forceFresh, reconfigure bool) error {
		if !forceFresh || !reconfigure {
			t.Fatalf("flags %v %v", forceFresh, reconfigure)
		}
		picked = append(picked, tsKey)
		return nil
	})
	if err := s.reconfigureTool(cfg, true); err != nil || saves != 1 || !reflect.DeepEqual(picked, []string{"tts"}) {
		t.Fatalf("picked %v saves %d %v", picked, saves, err)
	}
}

func TestToolsetEnabledForReconfigureScansAllowedPlatforms(t *testing.T) {
	s, _, _ := flowSetup(t)
	cfg := config.Tree{"platform_toolsets": map[string]any{}}
	for _, p := range Platforms {
		cfg["platform_toolsets"].(map[string]any)[p.Key] = []any{}
	}
	if s.toolsetEnabledForReconfigure("web", cfg) {
		t.Fatal("web enabled with empty lists")
	}
	cfg["platform_toolsets"].(map[string]any)["slack"] = []any{"web"}
	if !s.toolsetEnabledForReconfigure("web", cfg) {
		t.Fatal("web on slack not found")
	}
	cfg["platform_toolsets"].(map[string]any)["telegram"] = []any{"discord"}
	if s.toolsetEnabledForReconfigure("discord", cfg) {
		t.Fatal("discord counted on telegram")
	}
}

func TestCmdToolsRequiresATerminal(t *testing.T) {
	var errw bytes.Buffer
	swapFor(t, &toolsCommandStderr, io.Writer(&errw))
	swapFor(t, &toolsConfigConsoleFor, func(w io.Writer) *cliui.Console {
		return &cliui.Console{In: strings.NewReader(""), Out: w, StdinTTY: func() bool { return false }}
	})
	var out bytes.Buffer
	code, err := CmdTools(context.Background(), &Env{Home: t.TempDir()}, map[string]any{"tools_action": nil}, &out)
	if err != nil || code != 1 {
		t.Fatalf("code %d %v", code, err)
	}
	want := "Error: 'icebath tools' requires an interactive terminal.\nIt cannot be run through a pipe or non-interactive subprocess.\nRun it directly in your terminal instead.\n"
	if errw.String() != want || out.Len() != 0 {
		t.Fatalf("stderr %q stdout %q", errw.String(), out.String())
	}
}

func TestCmdToolsRoutesListEnableDisable(t *testing.T) {
	var calls [][]any
	swapFor(t, &toolsDisableEnableCommand, func(s *ToolsSetup, action, platform string, names []string) error {
		calls = append(calls, []any{action, platform, names, home.Dir(s.ctx())})
		return nil
	})
	h := t.TempDir()
	for _, ns := range []map[string]any{
		{"tools_action": "list", "platform": "cli"},
		{"tools_action": "disable", "platform": "telegram", "names": []string{"web", "github:create_issue"}},
		{"tools_action": "enable", "platform": "cli", "names": []string{"memory"}},
		{"tools_action": "list"},
	} {
		if code, err := CmdTools(context.Background(), &Env{Home: h}, ns, &bytes.Buffer{}); err != nil || code != 0 {
			t.Fatalf("%v: %d %v", ns, code, err)
		}
	}
	want := [][]any{{"list", "cli", []string(nil), h}, {"disable", "telegram", []string{"web", "github:create_issue"}, h}, {"enable", "cli", []string{"memory"}, h}, {"list", "cli", []string(nil), h}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

// Byte level vocabulary where every byte is its own token
func byteEncoding() *pytiktoken.Encoding {
	ranks := map[string]int{}
	for b := 0; b < 256; b++ {
		ranks[string([]byte{byte(b)})] = b
	}
	return pytiktoken.NewEncoding("bytes", `\s+|\S+`, ranks, map[string]int{"<|endoftext|>": 256})
}

func TestEstimateToolTokensCountsTheWireSchemaOncePerGeneration(t *testing.T) {
	s, _, _ := flowSetup(t)
	loads := 0
	swapFor(t, &toolTokenEncoding, func() (*pytiktoken.Encoding, error) { loads++; return byteEncoding(), nil })
	counts, err := estimateToolTokens(s.ctx())
	if err != nil || loads != 1 || len(counts) == 0 {
		t.Fatalf("counts %d loads %d %v", len(counts), loads, err)
	}
	name := Default.AllToolNames(s.ctx(), "")[0]
	wire := pyjson.NewObject()
	wire.Set("type", "function")
	wire.Set("function", Default.Schema(s.ctx(), "", name))
	text, _ := pyjson.Dumps(wire)
	if counts[name] != len(text) {
		t.Fatalf("%s %d want %d", name, counts[name], len(text))
	}
	if again, _ := estimateToolTokens(s.ctx()); loads != 1 || !reflect.DeepEqual(again, counts) {
		t.Fatalf("cache miss, loads %d", loads)
	}
}

func TestEstimateToolTokensWithoutTheEncodingIsEmptyAndCached(t *testing.T) {
	s, _, _ := flowSetup(t)
	loads := 0
	swapFor(t, &toolTokenEncoding, func() (*pytiktoken.Encoding, error) { loads++; return nil, os.ErrNotExist })
	for i := 0; i < 2; i++ {
		if counts, err := estimateToolTokens(s.ctx()); err != nil || len(counts) != 0 || loads != 1 {
			t.Fatalf("counts %v loads %d %v", counts, loads, err)
		}
	}
}
