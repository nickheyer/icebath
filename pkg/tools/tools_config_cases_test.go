package tools_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	_ "github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	_ "github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A configurator run over a temp home with captured output and no input
func casesSetup(t *testing.T) (*tools.ToolsSetup, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var out bytes.Buffer
	return &tools.ToolsSetup{Ctx: home.WithOverride(context.Background(), h), Con: &cliui.Console{In: strings.NewReader(""), Out: &out}}, &out
}

func sharedMetricsConfig(shared map[string]any) config.Tree {
	return config.Tree{"telemetry": map[string]any{"shared_metrics": shared}}
}

func sharedMetrics(cfg config.Tree) map[string]any {
	return cfg["telemetry"].(map[string]any)["shared_metrics"].(map[string]any)
}

func TestToggleEnablingSendPersists(t *testing.T) {
	s, _ := casesSetup(t)
	cfg := sharedMetricsConfig(map[string]any{"enabled": true})
	defer provider.SwapSetupTelemetryPrompt(func(string, bool, string) bool { return true })()
	var saved []config.Tree
	defer tools.SwapToolsConfigSaveForTest(func(_ *tools.ToolsSetup, c config.Tree) error { saved = append(saved, c); return nil })()
	if err := s.ConfigureSharedMetricsInteractiveForTest(cfg); err != nil {
		t.Fatal(err)
	}
	if sharedMetrics(cfg)["send"] != true {
		t.Fatalf("send %v", sharedMetrics(cfg))
	}
	if len(saved) == 0 {
		t.Fatal("a consent change must be written to disk")
	}
}

func TestToggleNoWriteWhenNothingChanged(t *testing.T) {
	s, _ := casesSetup(t)
	cfg := sharedMetricsConfig(map[string]any{"enabled": false, "send": false})
	defer provider.SwapSetupTelemetryPrompt(func(string, bool, string) bool { return false })()
	var saved []config.Tree
	defer tools.SwapToolsConfigSaveForTest(func(_ *tools.ToolsSetup, c config.Tree) error { saved = append(saved, c); return nil })()
	if err := s.ConfigureSharedMetricsInteractiveForTest(cfg); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("saved %v", saved)
	}
}

func TestToggleDisablingCollectionAlsoDisablesSending(t *testing.T) {
	s, _ := casesSetup(t)
	cfg := sharedMetricsConfig(map[string]any{"enabled": true, "send": true})
	defer provider.SwapSetupTelemetryPrompt(func(string, bool, string) bool { return false })()
	defer tools.SwapToolsConfigSaveForTest(func(*tools.ToolsSetup, config.Tree) error { return nil })()
	if err := s.ConfigureSharedMetricsInteractiveForTest(cfg); err != nil {
		t.Fatal(err)
	}
	shared := sharedMetrics(cfg)
	if shared["enabled"] != false || shared["send"] != false {
		t.Fatalf("shared %v", shared)
	}
}

func TestFirstInstallNousAutoConfiguresVideoGen(t *testing.T) {
	s, _ := casesSetup(t)
	cfg := config.Tree{"model": map[string]any{"provider": "nous"}, "platform_toolsets": map[string]any{"cli": []any{}}}
	for _, name := range []string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY", "ELEVENLABS_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL",
		"KEENABLE_API_KEY", "TAVILY_API_KEY", "PARALLEL_API_KEY", "BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "BROWSER_USE_API_KEY", "FAL_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	defer tools.SwapPromptToolsetChecklistForTest(func(*tools.ToolsSetup, string, map[string]bool, string, bool) (map[string]bool, error) {
		return map[string]bool{"video_gen": true}, nil
	})()
	defer tools.SwapToolsConfigSaveForTest(func(*tools.ToolsSetup, config.Tree) error { return nil })()
	defer tools.SwapGetEnabledPlatformsForTest(func(*tools.ToolsSetup) ([]string, error) { return []string{"cli"}, nil })()
	paid := true
	prev := provider.SwapNousSubAccountInfo(func(provider.PoolEnv, bool) *provider.NousPortalAccountInfo {
		return &provider.NousPortalAccountInfo{LoggedIn: true, Source: "jwt", Fresh: false, PaidServiceAccess: &paid}
	})
	defer provider.SwapNousSubAccountInfo(prev)
	var configured []string
	defer tools.SwapConfigureToolsetForTest(func(_ *tools.ToolsSetup, tsKey string, _ config.Tree, _, _ bool) error {
		configured = append(configured, tsKey)
		return nil
	})()
	if err := s.ToolsCommand(nil, true, cfg); err != nil {
		t.Fatal(err)
	}
	videoGen, _ := cfg["video_gen"].(map[string]any)
	if videoGen["provider"] != "nous" {
		t.Fatalf("video_gen %v", cfg["video_gen"])
	}
	if _, has := videoGen["use_gateway"]; has {
		t.Fatalf("use_gateway written %v", videoGen)
	}
	for _, ts := range configured {
		if ts == "video_gen" {
			t.Fatalf("video_gen configured manually %v", configured)
		}
	}
}

func TestPlatformToolsetConsistencySkillsConfigCoversToolsConfigPlatforms(t *testing.T) {
	for _, p := range tools.Platforms {
		if p.Key == "api_server" {
			continue
		}
		if _, ok := skills.SkillsConfigPlatformLabel(p.Key); !ok {
			t.Fatalf("Platform %q in tools_config but missing from skills_config PLATFORMS", p.Key)
		}
	}
}
