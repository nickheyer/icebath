package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type moaUsageFake struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type moaAcctFake struct {
	Model    string         `json:"model"`
	Messages []any          `json:"messages"`
	Output   string         `json:"output"`
	Usage    *moaUsageFake  `json:"usage"`
	CostUSD  float64        `json:"cost_usd"`
	Extra    map[string]any `json:"-"`
}

func TestSaveMoATurnAppendsOneRecordPerTurn(t *testing.T) {
	dir := t.TempDir()
	orig := loadMoAConfig
	t.Cleanup(func() { loadMoAConfig = orig })
	loadMoAConfig = func(context.Context) (map[string]any, error) { return map[string]any{"save_traces": false}, nil }
	SaveMoATurn(context.Background(), MoATurn{SessionID: "s"})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("tracing off writes nothing")
	}
	loadMoAConfig = func(context.Context) (map[string]any, error) {
		return map[string]any{"save_traces": true, "trace_dir": filepath.Join(dir, "traces")}, nil
	}
	acct := &moaAcctFake{Model: "ref-a", Messages: []any{"q"}, Output: "full answer", Usage: &moaUsageFake{InputTokens: 7, OutputTokens: 3}, CostUSD: 0.5}
	out := "agg ✓"
	turn := MoATurn{SessionID: "sess/1 ä", PresetName: "duo", ReferenceOutputs: []MoAReference{{Label: "A", Text: "prev", Acct: acct}},
		AggregatorLabel: "agg", AggregatorModel: "m", AggregatorInputMessages: []any{"x"}, AggregatorOutput: &out, AggregatorStreamed: true}
	SaveMoATurn(context.Background(), turn)
	turn.AggregatorOutput = nil
	SaveMoATurn(context.Background(), turn)
	data, err := os.ReadFile(filepath.Join(dir, "traces", "sess_1_ä.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %d", len(lines))
	}
	want := `"session_id": "sess/1 ä", "preset": "duo", "references": [{"label": "A", "model": "ref-a", "provider": null, "temperature": null, "input_messages": ["q"], "output": "full answer", "usage": {"input_tokens": 7, "output_tokens": 3, "cache_read_tokens": 0, "cache_write_tokens": 0, "reasoning_tokens": 0}, "cost_usd": 0.5, "cost_status": null, "cost_source": null}], "aggregator": {"label": "agg", "model": "m", "provider": null, "temperature": null, "input_messages": ["x"], "output": "agg ✓", "streamed": true, "output_location": "inline_from_stream"}}`
	if !strings.HasPrefix(lines[0], `{"ts": `) || !strings.HasSuffix(lines[0], want) {
		t.Fatalf("record %s", lines[0])
	}
	if !strings.Contains(lines[1], `"output": null, "streamed": true, "output_location": "assistant_message_in_session_db"`) {
		t.Fatalf("streamed without output %s", lines[1])
	}
	m := SlotMetrics(acct, "A", "redacted")
	if _, ok := m.Get("input_messages"); ok {
		t.Fatal("metrics drop the input messages")
	}
	if v, _ := m.Get("output"); v != "redacted" {
		t.Fatal("caller output wins")
	}
	if moaSanitizeSessionID("") != "unknown-session" {
		t.Fatal("empty session id")
	}
}
