package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

func TestHealStatsSurface(t *testing.T) {
	t.Run("test_debug_report_includes_heal_counters", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  sanitizer_heal_escalation_threshold: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		a, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{SessionID: "sess-report", APIKey: "test-key-1234567890", BaseURL: "http://127.0.0.1:1/v1",
			Provider: "custom", Model: "test-model", QuietMode: true, SkipContextFiles: true, SkipMemory: true})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		for range 2 {
			a.SanitizeAPIMessages([]map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": ""}, {"role": "user", "content": "next"}})
		}
		empty := LogSnapshot{}
		snapshots := map[string]LogSnapshot{}
		for _, name := range []string{"agent", "errors", "gateway", "gui", "desktop"} {
			snapshots[name] = empty
		}
		report, err := debugCollectDebugReport(ctx, 5, "dump", snapshots)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report, "sess-report") {
			t.Fatalf("report %q", report)
		}
	})
}
