package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

type scriptedEngine struct {
	stubEngine
	compress func(messages []map[string]any) []map[string]any
}

func (e *scriptedEngine) Compress(messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	e.compressCalled = true
	return e.compress(messages), nil
}

// Agent with no session store whose engine runs the scripted compression
func newCompressContextAgent(t *testing.T, compress func([]map[string]any) []map[string]any) (*AIAgent, *scriptedEngine, *[][2]string) {
	t.Helper()
	engine := &scriptedEngine{stubEngine: *newStubEngine(200000, 0.5), compress: compress}
	a := &AIAgent{sessionID: "sess-1", platform: "cli", model: "test-model", contextCompressor: engine}
	a.cachedSystemPrompt = "cached prompt"
	a.compressionFeasibilityChecked = true
	a.compressionInPlace = true
	a.compressionActivityHeartbeatInterval = 0.1
	events := &[][2]string{}
	a.statusCallback = func(kind, text string) { *events = append(*events, [2]string{kind, text}) }
	return a, engine, events
}

func compressTranscript() []map[string]any {
	return []map[string]any{{"role": "user", "content": "first"}, {"role": "assistant", "content": "reply"}, {"role": "user", "content": "second"}}
}

func TestCompressContextNoProgressHandsBackTheInput(t *testing.T) {
	a, engine, _ := newCompressContextAgent(t, func(m []map[string]any) []map[string]any { return m })
	messages := compressTranscript()
	system := "system"
	out, prompt, err := compressContext(context.Background(), a, messages, &system, compressContextOptions{Force: true})
	if err != nil || prompt != "cached prompt" || !reflect.DeepEqual(out, compressTranscript()) || !engine.compressCalled {
		t.Fatalf("out=%v prompt=%q err=%v called=%v", out, prompt, err, engine.compressCalled)
	}
	if !a.lastCompressionAttemptRecorded || a.lastCompressionAttemptInPlace != nil || a.compressionAttemptID == "" {
		t.Fatalf("attempt signals recorded=%v inPlace=%v id=%q", a.lastCompressionAttemptRecorded, a.lastCompressionAttemptInPlace, a.compressionAttemptID)
	}
}

func TestCompressContextEmptyTranscriptRefusesTheSplit(t *testing.T) {
	a, _, events := newCompressContextAgent(t, func([]map[string]any) []map[string]any { return []map[string]any{} })
	system := "system"
	out, prompt, err := compressContext(context.Background(), a, compressTranscript(), &system, compressContextOptions{Force: true})
	if err != nil || prompt != "cached prompt" || !reflect.DeepEqual(out, compressTranscript()) {
		t.Fatalf("out=%v prompt=%q err=%v", out, prompt, err)
	}
	want := [2]string{"warn", "⚠ Compression returned an empty transcript. No session split was performed; conversation continues unchanged."}
	if len(*events) == 0 || (*events)[len(*events)-1] != want {
		t.Fatalf("events = %v", *events)
	}
}

func TestCompressContextFenceCancelledAfterSummaryLeavesTheSessionAlone(t *testing.T) {
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{AuxiliaryTasks: func() []map[string]any { return nil }})
	fence := NewCompressionCommitFence()
	a, _, _ := newCompressContextAgent(t, func(m []map[string]any) []map[string]any {
		fence.CancelBeforeCommit(nil)
		return []map[string]any{{"role": "user", "content": "summary"}}
	})
	a.lastCompactionInPlace = true
	system := "system"
	out, prompt, err := compressContext(context.Background(), a, compressTranscript(), &system, compressContextOptions{Force: true, CommitFence: fence})
	if err != nil || prompt != "cached prompt" || !reflect.DeepEqual(out, compressTranscript()) || a.lastCompactionInPlace {
		t.Fatalf("out=%v prompt=%q err=%v inPlace=%v", out, prompt, err, a.lastCompactionInPlace)
	}
}

func TestCompressContextRefusesASecondDeferredNotification(t *testing.T) {
	a, engine, _ := newCompressContextAgent(t, func(m []map[string]any) []map[string]any { return m })
	a.pendingContextEngineCompressionNotification = func() bool { return true }
	system := "system"
	if _, _, err := compressContext(context.Background(), a, compressTranscript(), &system, compressContextOptions{DeferContextEngineNotification: true}); err == nil || err.Error() != "a compression notification is already pending" || engine.compressCalled {
		t.Fatalf("err=%v called=%v", err, engine.compressCalled)
	}
}
