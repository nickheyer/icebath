package memory

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs the real review fork with config.yaml text, a resolved runtime and a capturing agent hook
func reviewForkCapture(t *testing.T, cfgYAML string, rt *provider.Runtime) (CuratorReviewRequest, string) {
	t.Helper()
	ctx, h := curTestHome(t, nil)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	prevResolve, prevRun := curatorResolveRuntime, CuratorHooks.RunReview
	t.Cleanup(func() { curatorResolveRuntime, CuratorHooks.RunReview = prevResolve, prevRun })
	curatorResolveRuntime = func(context.Context, string, curatorReviewBinding) (*provider.Runtime, error) { return rt, nil }
	var captured CuratorReviewRequest
	calls := 0
	CuratorHooks.RunReview = func(_ context.Context, req CuratorReviewRequest) (*CuratorReviewResult, error) {
		captured = req
		calls++
		return &CuratorReviewResult{Conversation: map[string]any{"final_response": "ok"}}, nil
	}
	meta := curatorRunLLMReview(ctx, "review prompt")
	if calls != 1 {
		t.Fatalf("review fork ran %d times", calls)
	}
	errText := ""
	if v, ok := meta.Get("error"); ok && v != nil {
		errText = pyrepr.Str(v)
	}
	return captured, errText
}

func TestReviewForkForwardsRuntimeACPAndOverrides(t *testing.T) {
	pool := &provider.Pool{}
	req, errText := reviewForkCapture(t, "model:\n  provider: custom:hyper-charm\n  default: glm-5.2\n", &provider.Runtime{
		Provider: "custom", APIKey: "pool-token", BaseURL: "https://hyper.charm.land/v1", APIMode: "chat_completions",
		RequestOverrides: map[string]any{"extra_body": map[string]any{"store": false}}, Command: "copilot", Args: []string{"--acp", "--stdio"},
		CredentialPool: pool,
	})
	if errText != "" {
		t.Fatal(errText)
	}
	if pyrepr.Repr(req.RequestOverrides) != pyrepr.Repr(map[string]any{"extra_body": map[string]any{"store": false}}) {
		t.Fatalf("overrides %v", req.RequestOverrides)
	}
	if req.Route == nil || req.Route.APIKey != "pool-token" || req.Route.BaseURL != "https://hyper.charm.land/v1" || req.Route.Mode != "chat_completions" {
		t.Fatalf("route %+v", req.Route)
	}
	if req.CredentialPool != pool {
		t.Fatal("credential pool not forwarded")
	}
	if req.ACPCommand != "copilot" || !slices.Equal(req.ACPArgs, []string{"--acp", "--stdio"}) || req.Provider != "custom" || req.Model != "glm-5.2" {
		t.Fatalf("request %+v", req)
	}
	plain, _ := reviewForkCapture(t, "model:\n  provider: openai-api\n  default: gpt-4o-mini\n", &provider.Runtime{Provider: "openai-api", APIKey: "k"})
	if plain.ACPCommand != "" || plain.ACPArgs != nil || plain.CredentialPool != nil {
		t.Fatalf("a runtime without a command forwarded acp %q %v", plain.ACPCommand, plain.ACPArgs)
	}
}

func TestReviewForkReceivesConfiguredReasoning(t *testing.T) {
	req, errText := reviewForkCapture(t, "model:\n  provider: openai-api\n  default: gpt-4o-mini\nagent:\n  reasoning_effort: none\n",
		&provider.Runtime{Provider: "openai-api", APIKey: "k", BaseURL: "https://api.openai.com/v1", APIMode: "chat_completions"})
	if errText != "" {
		t.Fatal(errText)
	}
	if pyrepr.Repr(req.ReasoningConfig) != pyrepr.Repr(map[string]any{"enabled": false}) {
		t.Fatalf("reasoning %v", req.ReasoningConfig)
	}
}

func TestReviewForkUsesRuntimeModelAndOutputCap(t *testing.T) {
	req, errText := reviewForkCapture(t, "model:\n  provider: custom:gateway\n  default: gateway\n", &provider.Runtime{
		Provider: "custom", Model: "real-model-id", APIKey: "k", BaseURL: "https://gateway.example/v1", APIMode: "chat_completions",
	})
	if errText != "" || req.Model != "real-model-id" {
		t.Fatalf("%q model %q", errText, req.Model)
	}
}

func TestReviewForkRestrictsToolsetsToSkillsOnly(t *testing.T) {
	req, errText := reviewForkCapture(t, "model:\n  provider: custom:gateway\n  default: gateway\n", &provider.Runtime{
		Provider: "custom", Model: "m", APIKey: "k", BaseURL: "https://gateway.example/v1", APIMode: "chat_completions",
	})
	if errText != "" || !slices.Equal(req.EnabledToolsets, []string{"skills"}) {
		t.Fatalf("%q toolsets %v", errText, req.EnabledToolsets)
	}
	if req.MaxIterations != 9999 || req.Platform != "curator" || req.WriteOrigin != "background_review" || !req.QuietMode ||
		!req.SkipContextFiles || !req.SkipMemory || req.MemoryNudgeInterval != 0 || req.SkillNudgeInterval != 0 {
		t.Fatalf("request %+v", req)
	}
}

func TestReviewForkToolsetSurfaceExcludesExecutionTools(t *testing.T) {
	surface := tools.Default.ResolveStaticToolset(context.Background(), "", "skills")
	for _, tool := range []string{"skills_list", "skill_view", "skill_manage"} {
		if !slices.Contains(surface, tool) {
			t.Fatalf("%s missing from %v", tool, surface)
		}
	}
	for _, tool := range []string{"terminal", "process_manage", "write_file", "patch", "execute_code", "computer_use", "browser_exec"} {
		if slices.Contains(surface, tool) {
			t.Fatalf("execution or write tool %s leaked into the curator fork surface %v", tool, surface)
		}
	}
}
