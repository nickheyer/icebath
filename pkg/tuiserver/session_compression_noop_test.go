package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// test_compression_config_hot_reload.py test_unchanged_compression_config_is_noop
func TestUnchangedCompressionConfigIsNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	raw := "model:\n  context_length: 272000\ncompression:\n  threshold: 0.85\n  threshold_tokens: 100000\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a := testAgent(t, agent.AIAgentOptions{Model: "gpt-5.6-sol", Provider: "openai"})
	compressor, ok := a.ContextCompressor().(*agent.ContextCompressor)
	if !ok {
		t.Fatalf("context engine %T", a.ContextCompressor())
	}
	ctx := context.Background()
	session := tuigateway.NewSession(map[string]any{"agent": a, "session_key": "session-95151"})
	session.Set("config_compression_seen", tuiCompressionConfigSignature(ctx, loadCfg(ctx)))
	compressor.SetThresholdTokens(99_999)
	syncAgentCompressionWithConfig(ctx, "sid-95151", session)
	if got := compressor.ThresholdTokens(); got != 99_999 {
		t.Fatalf("threshold %d after an unchanged config", got)
	}
}
