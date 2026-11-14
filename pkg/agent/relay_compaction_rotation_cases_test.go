package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Ten exchanges of padded user and assistant rows numbered from start
func relayRotationTranscript(start int) []map[string]any {
	filler := strings.TrimSpace(strings.Repeat("context ", 200))
	var out []map[string]any
	for idx := start; idx < start+10; idx++ {
		out = append(out, map[string]any{"role": "user", "content": fmt.Sprintf("user message %d %s", idx, filler)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("assistant reply %d %s", idx, filler)})
	}
	return out
}

// Real agent on a rotating session whose compressor keeps a ten token tail
func relayRotatingAgent(t *testing.T, ctx context.Context) (*AIAgent, *ContextCompressor) {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", QuietMode: true,
		SessionDB: db, SessionID: "rotating-session", SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	a.compressionInPlace = false
	compressor, ok := a.contextCompressor.(*ContextCompressor)
	if !ok {
		t.Fatalf("context engine %T", a.contextCompressor)
	}
	budget := 10
	compressor.tailTokenBudget = &budget
	return a, compressor
}

func patchRelayRotationSummary(t *testing.T) {
	prev := compressorGenerateSummary
	compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
		s := "SANITIZED SUMMARY"
		return &s, nil
	}
	t.Cleanup(func() { compressorGenerateSummary = prev })
}

func relayRotationCompress(t *testing.T, ctx context.Context, a *AIAgent, messages []map[string]any) []map[string]any {
	t.Helper()
	sys, approx, trigger := "sys", 80_000, "turn_start_threshold"
	out, _, err := a.compressContext(ctx, messages, &sys, compressContextOptions{ApproxTokens: &approx, Trigger: &trigger})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryRotationInOneTurnIsMarkedOnThatTurn(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	a, _ := relayRotatingAgent(t, ctx)
	lease := relayCompactionAcquire(ctx, host, "rotating-session", "cli")
	turn := relayCompactionBegin(t, ctx, lease, "t1")
	noteTurnStart(a, "t1")
	patchRelayRotationSummary(t)
	first := relayRotationCompress(t, ctx, a, append([]map[string]any{{"role": "system", "content": "sys"}}, relayRotationTranscript(0)...))
	child := a.sessionID
	relayRotationCompress(t, ctx, a, append(first, relayRotationTranscript(10)...))
	noteTurnPersisted(a)
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	a.Close()
	if child == "rotating-session" || child == a.sessionID {
		t.Fatalf("sessions rotating-session -> %s -> %s", child, a.sessionID)
	}
	var marks [][2]any
	for _, event := range f.recorded() {
		name, _ := event["name"].(string)
		if !strings.HasPrefix(name, "compaction") {
			continue
		}
		data, _ := event["data"].(map[string]any)
		marks = append(marks, [2]any{name, data["session_id"]})
		if !tools.RelayIdentical(event["handle"], turn.Handle) || data["split_status"] != "rotated_committed" {
			t.Fatalf("mark %v", event)
		}
	}
	if !reflect.DeepEqual(marks, [][2]any{{"compaction", "rotating-session"}, {"compaction", child}}) {
		t.Fatalf("marks %v", marks)
	}
}

func TestAPostTurnMicroCompactionInARotatedChildIsMarkedOnTheTurn(t *testing.T) {
	f, ctx, host := relayCompactionHost(t)
	a, compressor := relayRotatingAgent(t, ctx)
	lease := relayCompactionAcquire(ctx, host, "rotating-session", "cli")
	turn := relayCompactionBegin(t, ctx, lease, "t1")
	noteTurnStart(a, "t1")
	patchRelayRotationSummary(t)
	compacted := relayRotationCompress(t, ctx, a, append([]map[string]any{{"role": "system", "content": "sys"}}, relayRotationTranscript(0)...))
	child := a.sessionID
	compressor.microCompactEnabled = true
	prev := compressorMicroSummarizeOne
	compressorMicroSummarizeOne = func(*ContextCompressor, string) (*string, error) {
		s := "ROLLING SUMMARY"
		return &s, nil
	}
	t.Cleanup(func() { compressorMicroSummarizeOne = prev })
	var live []*state.Message
	for _, m := range append(compacted, relayRotationTranscript(10)...) {
		live = append(live, state.MessageFromDict(m))
	}
	microCompactAfterTurn(ctx, a, &live, "done", "task")
	noteTurnPersisted(a)
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	a.Close()
	if child == "rotating-session" {
		t.Fatal("the commit never rotated")
	}
	var micro [][2]any
	for _, event := range f.recorded() {
		data, _ := event["data"].(map[string]any)
		if data["kind"] != "micro_summarize" {
			continue
		}
		if !tools.RelayIdentical(event["handle"], turn.Handle) {
			t.Fatalf("micro mark off the turn %v", event)
		}
		micro = append(micro, [2]any{data["session_id"], "turn"})
	}
	if !reflect.DeepEqual(micro, [][2]any{{child, "turn"}}) {
		t.Fatalf("micro marks %v", micro)
	}
}
