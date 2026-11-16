package app

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
)

// A CLI built without its constructor whose session started fourteen and a half minutes ago
func statusBarCLI(t *testing.T, fields []any) *CLI {
	t.Helper()
	recordPrints(t)
	display := map[string]any{}
	if fields != nil {
		display["status_bar"] = map[string]any{"fields": fields}
	}
	SetCLIConfig(map[string]any{"display": display})
	t.Cleanup(func() { SetCLIConfig(nil) })
	c := bareCLI()
	c.model = "anthropic/claude-sonnet-4-20250514"
	c.sessionStart = time.Now().Add(-(14*time.Minute + 32*time.Second))
	c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}}
	return c
}

type usageSpec struct {
	prompt, completion, total, calls, contextTokens, contextLength, compressions, cacheRead, cacheWrite int
	latencies                                                                                           []float64
	outputs                                                                                             []int
}

// Attaches an agent whose status bar usage the test owns and may change between repaints
func attachUsage(t *testing.T, c *CLI, spec *usageSpec) {
	t.Helper()
	c.setAgent(&GatewayAgent{ctx: context.Background()})
	prev := statusBarUsage
	statusBarUsage = func(*CLI, cliAgent) agent.StatusBarUsage {
		usage := agent.StatusBarUsage{Counters: map[string]int{
			"session_input_tokens": spec.prompt, "session_output_tokens": spec.completion, "session_cache_read_tokens": spec.cacheRead,
			"session_cache_write_tokens": spec.cacheWrite, "session_prompt_tokens": spec.prompt, "session_completion_tokens": spec.completion,
			"session_total_tokens": spec.total, "session_api_calls": spec.calls},
			HasCompressor: true, ContextTokens: spec.contextTokens, ContextLength: spec.contextLength, Compressions: spec.compressions,
			LatencyHistory: spec.latencies, OutputHistory: spec.outputs}
		if spec.contextLength != 0 {
			pct := max(0, min(100, int(math.RoundToEven(float64(spec.contextTokens)/float64(spec.contextLength)*100))))
			usage.ContextPercent = &pct
		}
		return usage
	}
	t.Cleanup(func() { statusBarUsage = prev })
}

func baseUsage() *usageSpec {
	return &usageSpec{prompt: 10_230, completion: 2_220, total: 12_450, calls: 7, contextTokens: 12_450, contextLength: 200_000}
}

func cacheUsage(read, write int) *usageSpec {
	return &usageSpec{prompt: 10_000, completion: 2_000, total: 12_000, calls: 5, contextTokens: 12_000, contextLength: 200_000, cacheRead: read, cacheWrite: write}
}

func fixWidth(t *testing.T, width int) {
	t.Helper()
	prev := tuiTerminalWidthFor
	tuiTerminalWidthFor = func(*CLI) int { return width }
	t.Cleanup(func() { tuiTerminalWidthFor = prev })
}

func TestIdleSinceLastTurn(t *testing.T) {
	t.Run("test_hidden_before_first_turn", func(t *testing.T) {
		if formatIdleSince(nil, false) != "" {
			t.Fatal("no turn has finished yet")
		}
	})
	t.Run("test_hidden_while_turn_is_live", func(t *testing.T) {
		at := wallClock() - 30
		if formatIdleSince(&at, true) != "" {
			t.Fatal("a live turn hides the idle time")
		}
	})
	t.Run("test_shows_compact_idle_time_after_turn", func(t *testing.T) {
		at := wallClock() - 42
		if got := formatIdleSince(&at, false); got != "✓ 42s" {
			t.Fatalf("label %q", got)
		}
	})
}
