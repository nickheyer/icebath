package observability_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	_ "github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

func strPtr(s string) *string { return &s }

func TestSourceCheckoutChannelFollowsItsBranchNeverItsRemote(t *testing.T) {
	for _, c := range []struct {
		branch  *string
		channel string
	}{{strPtr("main"), "main"}, {strPtr("master"), "main"}, {strPtr("feat/x"), "dev"}, {nil, "unknown"}} {
		observability.WithProjectRootForTest(t, t.TempDir())
		observability.WithVersionBranchForTest(t, c.branch)
		if got, err := observability.ReleaseChannel(map[string]any{}); err != nil || got != c.channel {
			t.Fatalf("branch %v read %s %v want %s", c.branch, got, err, c.channel)
		}
	}
}

func TestPackagedBuildChannelIsItsBakedChannelNotItsBranch(t *testing.T) {
	for _, c := range []struct{ tag, channel string }{{"v0.13.0", "stable"}, {"v0.13.0+canary.20260927T120000Z", "main"}} {
		if c.channel == "main" && !selfupdate.IsCanaryTag(c.tag) {
			t.Fatalf("%s is not a canary tag", c.tag)
		}
		root := t.TempDir()
		stamp, _ := json.Marshal(map[string]any{"updateMechanism": "electron-updater", "tag": c.tag})
		if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), stamp, 0o600); err != nil {
			t.Fatal(err)
		}
		observability.WithProjectRootForTest(t, root)
		observability.WithVersionBranchForTest(t, strPtr("feat/x"))
		if got, err := observability.ReleaseChannel(map[string]any{}); err != nil || got != c.channel {
			t.Fatalf("%s read %s %v want %s", c.tag, got, err, c.channel)
		}
	}
}

type localModelCase struct {
	cfg  map[string]any
	used string
}

func assertLocalModelUse(t *testing.T, cases []localModelCase) {
	t.Helper()
	ctx := observability.DirectRuntimeForTest(t)
	for _, c := range cases {
		if got, err := observability.LocalModelProviderUsed(ctx, c.cfg); err != nil || got != c.used {
			t.Fatalf("%v read %s %v want %s", c.cfg, got, err, c.used)
		}
	}
}

func TestLocalModelUseCoversMainAndAuxiliarySlots(t *testing.T) {
	assertLocalModelUse(t, []localModelCase{
		{map[string]any{"model": map[string]any{"provider": "openrouter", "default": "x"}}, "no"},
		{map[string]any{"model": map[string]any{"provider": "ollama"}}, "yes"},
		{map[string]any{"model": map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}}, "yes"},
		{map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.example.com/v1"}}, "no"},
		{map[string]any{"model": map[string]any{"provider": "openrouter"}, "auxiliary": map[string]any{"vision": map[string]any{"provider": "lmstudio"}}}, "yes"},
		{map[string]any{"model": map[string]any{"provider": "openrouter"}, "auxiliary": map[string]any{"compression": map[string]any{"provider": "auto", "base_url": "http://localhost:1"}}}, "no"},
		{map[string]any{}, "no"},
	})
}

func TestLocalModelUseSeesABareAuxiliaryEndpoint(t *testing.T) {
	var cases []localModelCase
	for _, c := range []struct {
		aux  map[string]any
		used string
	}{
		{map[string]any{"provider": "auto", "base_url": "http://localhost:11434/v1", "api_key": "k", "model": "q"}, "yes"},
		{map[string]any{"base_url": "http://127.0.0.1:8080/v1", "api_key": "k", "model": "q"}, "yes"},
		{map[string]any{"base_url": "https://api.example.com/v1", "api_key": "k", "model": "q"}, "no"},
	} {
		cases = append(cases, localModelCase{map[string]any{"model": map[string]any{"provider": "openrouter"}, "auxiliary": map[string]any{"vision": c.aux}}, c.used})
	}
	assertLocalModelUse(t, cases)
}

func TestLocalModelUseResolvesANamedProviderUsedByBareName(t *testing.T) {
	for _, c := range []struct{ baseURL, used string }{{"http://127.0.0.1:8080/v1", "yes"}, {"https://llm.example.com/v1", "no"}} {
		t.Run(c.used, func(t *testing.T) {
			ctx := observability.DirectRuntimeForTest(t)
			body := "model:\n  provider: home-llama\n  default: q\nproviders:\n  home-llama:\n    base_url: " + c.baseURL + "\n    api_key: k\n"
			if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"model": map[string]any{"provider": "home-llama", "default": "q"},
				"providers": map[string]any{"home-llama": map[string]any{"base_url": c.baseURL, "api_key": "k"}}}
			if got, err := observability.LocalModelProviderUsed(ctx, cfg); err != nil || got != c.used {
				t.Fatalf("%s read %s %v want %s", c.baseURL, got, err, c.used)
			}
		})
	}
}
