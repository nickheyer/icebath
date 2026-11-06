package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

type fakeFeasibilityClient struct{ baseURL, apiKey string }

func (c *fakeFeasibilityClient) BaseURL() string { return c.baseURL }
func (c *fakeFeasibilityClient) APIKey() string  { return c.apiKey }
func (c *fakeFeasibilityClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return nil, errors.New("unused")
}
func (c *fakeFeasibilityClient) Close() error { return nil }

func feasibilityCompressorOptions(model string, window int, pct float64, tailMode string) ContextCompressorOptions {
	o := DefaultContextCompressorOptions(model)
	o.QuietMode, o.ConfigContextLength, o.ThresholdPercent, o.TailMode = true, &window, pct, tailMode
	return o
}

type feasibilityProbe struct {
	fallbackCalls [][2]string
	ctxRequests   []provider.ContextLengthRequest
}

// Agent over a real compressor with the auxiliary route, fallback and window lookups patched
func newFeasibilityAgent(t *testing.T, window int, pct float64, tailMode string, client provider.AuxClient, auxModel string, auxContext int) (*AIAgent, *ContextCompressor, *feasibilityProbe) {
	t.Helper()
	a := &AIAgent{model: "test-main-model", provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", compressionEnabled: true}
	c := NewContextCompressor(context.Background(), feasibilityCompressorOptions("test-main-model", window, pct, tailMode))
	a.contextCompressor = c
	probe := &feasibilityProbe{}
	prev := [4]any{feasibilityTaskProvider, feasibilityAuxClient, feasibilityFallbackClient, feasibilityContextLength}
	feasibilityTaskProvider = func(context.Context, string) string { return "auto" }
	feasibilityAuxClient = func(context.Context, string, map[string]any) (provider.AuxClient, string, error) {
		if client == nil {
			return nil, "", nil
		}
		return client, auxModel, nil
	}
	feasibilityFallbackClient = func(_ context.Context, task, failed string) (provider.AuxClient, string, string, error) {
		probe.fallbackCalls = append(probe.fallbackCalls, [2]string{task, failed})
		return nil, "", "", nil
	}
	feasibilityContextLength = func(_ context.Context, req provider.ContextLengthRequest) int {
		probe.ctxRequests = append(probe.ctxRequests, req)
		return auxContext
	}
	t.Cleanup(func() {
		feasibilityTaskProvider = prev[0].(func(context.Context, string) string)
		feasibilityAuxClient = prev[1].(func(context.Context, string, map[string]any) (provider.AuxClient, string, error))
		feasibilityFallbackClient = prev[2].(func(context.Context, string, string) (provider.AuxClient, string, string, error))
		feasibilityContextLength = prev[3].(func(context.Context, provider.ContextLengthRequest) int)
	})
	return a, c, probe
}

func TestAuxSyncKeepsLeanTailPolicy(t *testing.T) {
	for _, tc := range [][2]int{{1_000_000, 512_000}, {400_000, 80_000}} {
		client := &fakeFeasibilityClient{baseURL: "http://localhost/v1", apiKey: "test-key"}
		a, c, _ := newFeasibilityAgent(t, tc[0], 0.85, "lean", client, "aux", tc[1])
		before := c.TailTokenBudget()
		if err := checkCompressionModelFeasibility(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if c.ThresholdTokens() != tc[1] || c.TailTokenBudget() != before {
			t.Fatalf("%v: threshold=%d tail=%d before=%d", tc, c.ThresholdTokens(), c.TailTokenBudget(), before)
		}
		_ = checkCompressionModelFeasibility(context.Background(), a)
		if c.TailTokenBudget() != before {
			t.Fatalf("%v: repeated probe moved the lean tail", tc)
		}
		c.UpdateModel("test-main-model", tc[0], "", "", "", "")
		if c.TailTokenBudget() != before {
			t.Fatalf("%v: recalibration moved the lean tail to %d", tc, c.TailTokenBudget())
		}
	}
}

func TestAuxSyncLegacyTailFollowsLoweredThreshold(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "http://localhost/v1", apiKey: "test-key"}
	a, c, _ := newFeasibilityAgent(t, 1_000_000, 0.85, "legacy", client, "aux", 512_000)
	before := c.TailTokenBudget()
	_ = checkCompressionModelFeasibility(context.Background(), a)
	if c.ThresholdTokens() != 512_000 || c.TailTokenBudget() >= before || c.TailTokenBudget() != int(float64(c.ThresholdTokens())*c.summaryTargetRatio) {
		t.Fatalf("threshold=%d tail=%d before=%d", c.ThresholdTokens(), c.TailTokenBudget(), before)
	}
}

func TestAutoCorrectsThresholdWhenAuxContextBelowThreshold(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "https://openrouter.ai/api/v1", apiKey: "sk-aux"}
	a, c, _ := newFeasibilityAgent(t, 200_000, 0.50, "legacy", client, "google/gemini-3-flash-preview", 80_000)
	c.SetThresholdTokens(100_000)
	c.SetTailTokenBudget(20_000)
	_ = checkCompressionModelFeasibility(context.Background(), a)
	msg, _ := a.compressionWarning.(string)
	for _, want := range []string{"Compression model", "80,000", "100,000", "Auto-lowered", "config.yaml", "auxiliary:", "compression:", "150,000"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("warning lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "threshold:") {
		t.Fatalf("unviable threshold suggestion offered: %s", msg)
	}
	if c.ThresholdTokens() != 80_000 || c.TailTokenBudget() != 16_000 {
		t.Fatalf("threshold=%d tail=%d", c.ThresholdTokens(), c.TailTokenBudget())
	}
}

func TestRejectsAuxBelowMinimumContext(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "https://openrouter.ai/api/v1"}
	a, _, _ := newFeasibilityAgent(t, 200_000, 0.50, "lean", client, "tiny-aux-model", 32_768)
	err := checkCompressionModelFeasibility(context.Background(), a)
	var below *auxContextBelowMinimumError
	if !errors.As(err, &below) || !strings.Contains(err.Error(), "tiny-aux-model") {
		t.Fatalf("err = %v", err)
	}
	revalidateCompressionFeasibility(context.Background(), a)
	if a.compressionFeasibilityChecked {
		t.Fatalf("a hard rejection latched the probe")
	}
}

func TestWarnsWhenNoAuxiliaryProvider(t *testing.T) {
	a, _, _ := newFeasibilityAgent(t, 200_000, 0.50, "lean", nil, "", 0)
	_ = checkCompressionModelFeasibility(context.Background(), a)
	if msg, _ := a.compressionWarning.(string); !strings.Contains(msg, "No auxiliary LLM provider configured") {
		t.Fatalf("warning = %v", a.compressionWarning)
	}
}

func TestNoUnavailableWarningWhenConfiguredFallbackChainResolves(t *testing.T) {
	a, _, probe := newFeasibilityAgent(t, 200_000, 0.50, "lean", nil, "", 200_000)
	feasibilityTaskProvider = func(context.Context, string) string { return "ollama-cloud" }
	fallback := &fakeFeasibilityClient{baseURL: "https://chatgpt.com/backend-api/codex", apiKey: "codex-oauth-token"}
	feasibilityFallbackClient = func(_ context.Context, task, failed string) (provider.AuxClient, string, string, error) {
		probe.fallbackCalls = append(probe.fallbackCalls, [2]string{task, failed})
		return fallback, "gpt-5.4-mini", "fallback_chain[0](openai-codex)", nil
	}
	_ = checkCompressionModelFeasibility(context.Background(), a)
	if a.compressionWarning != nil || len(probe.fallbackCalls) != 1 || probe.fallbackCalls[0] != [2]string{"compression", "ollama-cloud"} {
		t.Fatalf("warning=%v fallback=%v", a.compressionWarning, probe.fallbackCalls)
	}
	if len(probe.ctxRequests) != 1 || probe.ctxRequests[0].Model != "gpt-5.4-mini" || probe.ctxRequests[0].Provider != "openai-codex" {
		t.Fatalf("window lookups = %+v", probe.ctxRequests)
	}
}

func TestWarningStoredForGatewayReplay(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "https://openrouter.ai/api/v1"}
	a, _, _ := newFeasibilityAgent(t, 200_000, 0.50, "lean", client, "aux", 80_000)
	_ = checkCompressionModelFeasibility(context.Background(), a)
	var got []any
	a.statusCallback = func(kind string, msg any) { got = append(got, kind, msg) }
	replayCompressionWarning(a)
	if len(got) != 2 || got[0] != "lifecycle" {
		t.Fatalf("replayed %v", got)
	}
	if text, ok := got[1].(config.DiagnosticText); !ok || string(text) != a.compressionWarning {
		t.Fatalf("replayed value %#v", got[1])
	}
}

func TestNoReplayWhenNoWarning(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "https://openrouter.ai/api/v1"}
	a, _, _ := newFeasibilityAgent(t, 200_000, 0.50, "lean", client, "aux", 1_000_000)
	_ = checkCompressionModelFeasibility(context.Background(), a)
	calls := 0
	a.statusCallback = func(string, any) { calls++ }
	replayCompressionWarning(a)
	if calls != 0 || a.compressionWarning != nil {
		t.Fatalf("calls=%d warning=%v", calls, a.compressionWarning)
	}
}

func TestEnsureFeasibilityProbeWaitsForTheMinimumWindow(t *testing.T) {
	client := &fakeFeasibilityClient{baseURL: "https://openrouter.ai/api/v1"}
	a, c, probe := newFeasibilityAgent(t, 200_000, 0.50, "lean", client, "aux", 80_000)
	ensureCompressionFeasibilityChecked(context.Background(), a, provider.MinimumContextLength-1)
	if a.compressionFeasibilityChecked || len(probe.ctxRequests) != 0 {
		t.Fatalf("probed below the minimum window")
	}
	ensureCompressionFeasibilityChecked(context.Background(), a, provider.MinimumContextLength)
	if !a.compressionFeasibilityChecked || c.ThresholdTokens() != 80_000 {
		t.Fatalf("checked=%v threshold=%d", a.compressionFeasibilityChecked, c.ThresholdTokens())
	}
}
