package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func routingAgent(providerName, baseURL string) *AIAgent {
	return &AIAgent{provider: providerName, baseURL: baseURL, baseURLLower: strings.ToLower(baseURL), model: "gpt-5.4", apiMode: "chat_completions"}
}

func TestResponsesUpgradeIsSkippedByACPSchemeNotVendorSlug(t *testing.T) {
	ctx := context.Background()
	for _, tc := range [][2]string{{"copilot-acp", "acp://copilot"}, {"acme-acp", "acp://acme"}, {"acme-acp", "acp+tcp://127.0.0.1:9000"}} {
		a := routingAgent(tc[0], tc[1])
		a.finalizeRouting(ctx, "")
		if a.apiMode != "chat_completions" {
			t.Fatalf("%v api_mode = %q", tc, a.apiMode)
		}
	}
	upgraded := routingAgent("acme-acp", "https://api.openai.com/v1")
	upgraded.transportCache = map[string]provider.Transport{"chat_completions": nil}
	upgraded.finalizeRouting(ctx, "")
	if upgraded.apiMode != "codex_responses" {
		t.Fatalf("api_mode = %q", upgraded.apiMode)
	}
	if len(upgraded.transportCache) != 0 {
		t.Fatalf("transport cache kept %v after the upgrade", upgraded.transportCache)
	}
}

func TestResponsesUpgradeIsSkippedForExternalProcessProfileOnAnyBaseURL(t *testing.T) {
	ctx := context.Background()
	provider.RegisterProvider(provider.Profile{Name: "routing-test-process-acp", AuthType: provider.AuthTypeExternalProcess})
	a := routingAgent("routing-test-process-acp", "https://proxy.example.invalid/v1")
	a.finalizeRouting(ctx, "")
	if a.apiMode != "chat_completions" {
		t.Fatalf("external process api_mode = %q", a.apiMode)
	}
	plain := routingAgent("acme-http", "https://proxy.example.invalid/v1")
	plain.finalizeRouting(ctx, "")
	if plain.apiMode != "codex_responses" {
		t.Fatalf("plain api_mode = %q", plain.apiMode)
	}
}

func TestExplicitAPIModeIsNeverUpgraded(t *testing.T) {
	a := routingAgent("acme-http", "https://api.openai.com/v1")
	a.finalizeRouting(context.Background(), "chat_completions")
	if a.apiMode != "chat_completions" {
		t.Fatalf("api_mode = %q", a.apiMode)
	}
}

func TestKeepaliveThreadStartsWhenAnAgentRoutesToNous(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var started atomic.Int32
	prev := nousKeepaliveStart
	nousKeepaliveStart = func(provider.PoolEnv, provider.NousKeepaliveStartOptions) bool { started.Add(1); return true }
	t.Cleanup(func() { nousKeepaliveStart = prev })
	nous := &AIAgent{provider: "nous", baseURL: "https://inference-api.nousresearch.com/v1", model: "anthropic/claude-fable-5.1", apiMode: "chat_completions"}
	nous.finalizeRouting(context.Background(), "")
	if started.Load() != 1 {
		t.Fatalf("started = %d after the nous agent", started.Load())
	}
	or := &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", model: "anthropic/claude-fable-5.1", apiMode: "chat_completions"}
	or.finalizeRouting(context.Background(), "")
	if started.Load() != 1 {
		t.Fatalf("started = %d after the openrouter agent", started.Load())
	}
}

func TestOpenRouterMetadataPrewarmRunsOncePerProcess(t *testing.T) {
	prevFn, prevDone := openrouterPrewarmFn, openrouterPrewarmDone.Load()
	calls := make(chan struct{}, 4)
	openrouterPrewarmFn = func(context.Context) { calls <- struct{}{} }
	openrouterPrewarmDone.Store(false)
	t.Cleanup(func() { openrouterPrewarmFn = prevFn; openrouterPrewarmDone.Store(prevDone) })
	for range 3 {
		a := &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", baseURLLower: "https://openrouter.ai/api/v1", model: "anthropic/claude-fable-5.1", apiMode: "chat_completions"}
		a.finalizeRouting(context.Background(), "")
	}
	<-calls
	if len(calls) != 0 || !openrouterPrewarmDone.Load() {
		t.Fatalf("prewarm ran %d extra times, done=%v", len(calls), openrouterPrewarmDone.Load())
	}
}

func TestMismatchedCredentialPoolIsDroppedAtRouting(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", model: "x/y", apiMode: "chat_completions", credentialPool: provider.NewPool(agentPoolEnv(context.Background()), "anthropic", nil)}
	openrouterPrewarmDone.Store(true)
	a.finalizeRouting(context.Background(), "")
	if a.credentialPool != nil {
		t.Fatal("an anthropic pool survived on an openrouter route")
	}
	b := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages", credentialPool: provider.NewPool(agentPoolEnv(context.Background()), "anthropic", nil)}
	b.finalizeRouting(context.Background(), "")
	if b.credentialPool == nil {
		t.Fatal("a matching pool was dropped")
	}
}
