package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Builds a codex agent through the real init with extra compression keys in the profile config
func makeCompressionKeysAgent(t *testing.T, extra, sessionID string) *AIAgent {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	body := "compression:\n  enabled: true\n  threshold: 0.50\n  target_ratio: 0.20\n  protect_first_n: 3\n  protect_last_n: 20\n" +
		extra + "prompt_caching:\n  cache_ttl: 5m\nsessions: {}\nbedrock: {}\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	captureStatusStdout(t)
	db, err := state.Open(filepath.Join(h, "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := NewAIAgent(context.Background(), AIAgentOptions{
		BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "test-key", Provider: "openai-codex", Model: "gpt-5.5",
		EnabledToolsets: []string{}, DisabledToolsets: []string{}, QuietMode: true, SkipMemory: true, SessionDB: db, SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestCompressionMaxAttemptsConfigCustomValueIsHonored(t *testing.T) {
	a := makeCompressionKeysAgent(t, "  max_attempts: 6\n", "max-attempts-test")
	if a.maxCompressionAttempts != 6 {
		t.Fatalf("max_compression_attempts %d", a.maxCompressionAttempts)
	}
}

func TestProactivePruneConfigCustomValuesAreHonored(t *testing.T) {
	a := makeCompressionKeysAgent(t, "  proactive_prune_tokens: 48000\n  proactive_prune_min_result_chars: 12000\n  proactive_prune_min_reclaim_tokens: 8192\n", "proactive-prune-config-test")
	cc, ok := a.contextCompressor.(*ContextCompressor)
	if !ok {
		t.Fatalf("context compressor %T", a.contextCompressor)
	}
	if cc.proactivePruneTokens != 48_000 || cc.proactivePruneMinResultChars != 12_000 || cc.proactivePruneMinReclaimTokens != 8_192 {
		t.Fatalf("prune %d %d %d", cc.proactivePruneTokens, cc.proactivePruneMinResultChars, cc.proactivePruneMinReclaimTokens)
	}
}

func TestProactivePruneConfigBooleanIsRejectedNotCoerced(t *testing.T) {
	a := makeCompressionKeysAgent(t, "  proactive_prune_tokens: true\n", "proactive-prune-config-test")
	cc, ok := a.contextCompressor.(*ContextCompressor)
	if !ok {
		t.Fatalf("context compressor %T", a.contextCompressor)
	}
	if cc.proactivePruneTokens != 0 {
		t.Fatalf("proactive_prune_tokens %d", cc.proactivePruneTokens)
	}
}
