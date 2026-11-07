package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
)

type sessionEndRecorder struct {
	memory.BaseProvider
	ends int
}

func (r *sessionEndRecorder) Name() string                   { return "builtin" }
func (r *sessionEndRecorder) Available(context.Context) bool { return true }
func (r *sessionEndRecorder) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (r *sessionEndRecorder) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (r *sessionEndRecorder) OnSessionEnd(context.Context, []map[string]any) error {
	r.ends++
	return nil
}

// Agent over a fresh session store whose built in compressor runs the patched compress pass
func newRefusalAgent(t *testing.T, sessionID string, compress func(c *ContextCompressor, msgs []map[string]any) ([]map[string]any, error)) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	c := NewContextCompressor(context.Background(), feasibilityCompressorOptions("test/model", 200000, 0.5, "lean"))
	prev := compressorCompress
	compressorCompress = func(c *ContextCompressor, _ context.Context, msgs []map[string]any, _ *int, _ *string, _ bool, _ string, _ bool) ([]map[string]any, error) {
		return compress(c, msgs)
	}
	prevCall := compressorCallLLM
	compressorCallLLM = func(context.Context, map[string]any) (any, error) { return nil, errors.New("Connection error.") }
	t.Cleanup(func() { compressorCompress, compressorCallLLM = prev, prevCall })
	a := &AIAgent{sessionID: sessionID, platform: "cli", model: "test/model", contextCompressor: c, sessionDB: db}
	a.cachedSystemPrompt = "cached prompt"
	a.compressionFeasibilityChecked = true
	a.compressionActivityHeartbeatInterval = 0.1
	rec := &sessionEndRecorder{}
	a.memoryManager = memory.NewMemoryManager(context.Background(), nil, nil)
	if err := a.memoryManager.AddProvider(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rec.ends != 0 {
			t.Errorf("commit_memory_session ran %d times on a refused pass", rec.ends)
		}
	})
	return a, db
}

func refusalTranscript() []map[string]any {
	out := make([]map[string]any, 21)
	for i := range out {
		out[i] = map[string]any{"role": "user", "content": fmt.Sprintf("m%d ", i) + strings.Repeat("x", 200)}
	}
	return out
}

// Overwrites every row of the live list the way a legacy engine clears the list it receives
func clearInPlace(msgs []map[string]any) {
	for i := range msgs {
		msgs[i] = map[string]any{"role": "user", "content": "cleared"}
	}
}

// Session row, child, end reason and lock holder all untouched by a refused pass
func assertSessionUntouched(t *testing.T, a *AIAgent, db *state.SessionDB, sessionID string) {
	t.Helper()
	if a.sessionID != sessionID {
		t.Fatalf("session rotated to %q", a.sessionID)
	}
	child, err := db.FindLiveCompressionChild(context.Background(), sessionID)
	if err != nil || child != nil {
		t.Fatalf("child=%v err=%v", child, err)
	}
	row, err := db.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if reason, _ := row.Get("end_reason"); reason != nil {
		t.Fatalf("end_reason = %v", reason)
	}
	if holder, err := db.GetCompressionLockHolder(sessionID); err != nil || holder != nil {
		t.Fatalf("lock holder %v err=%v", holder, err)
	}
	stored, err := db.GetMessages(context.Background(), sessionID, state.GetMessagesOptions{})
	if err != nil || len(stored) != 0 {
		t.Fatalf("archive_and_compact wrote %d rows err=%v", len(stored), err)
	}
}

func TestRefusedCandidateFromMutatingEngineReturnsOriginalTranscript(t *testing.T) {
	grown := strings.Repeat("GROWN ", 20000)
	engines := map[string]func(c *ContextCompressor, msgs []map[string]any) ([]map[string]any, error){
		"compressor_aborted": func(c *ContextCompressor, msgs []map[string]any) ([]map[string]any, error) {
			clearInPlace(msgs)
			c.lastCompressAborted = true
			c.SetLastSummaryError("summary provider unavailable")
			return msgs[:0], nil
		},
		"empty_transcript": func(_ *ContextCompressor, msgs []map[string]any) ([]map[string]any, error) {
			clearInPlace(msgs)
			return []map[string]any{}, nil
		},
		"would_grow": func(c *ContextCompressor, msgs []map[string]any) ([]map[string]any, error) {
			tokens := 120000
			c.beginCompressionTelemetry(&tokens)
			c.Last = c.Active
			c.Active.Set("messages_after", 2)
			c.Active.Set("tokens_after", 9)
			c.Active.Set("tokens_reclaimed", 99)
			kept := append([]map[string]any(nil), msgs[:len(msgs)-1]...)
			msgs[len(msgs)-1] = map[string]any{"role": "user", "content": "popped"}
			return append(kept, map[string]any{"role": "assistant", "content": grown}), nil
		},
		"would_grow_in_place": func(_ *ContextCompressor, msgs []map[string]any) ([]map[string]any, error) {
			msgs[0] = map[string]any{"role": "assistant", "content": grown}
			return msgs[:1], nil
		},
	}
	for name, engine := range engines {
		t.Run(name, func(t *testing.T) {
			records := captureAttemptRecords(t)
			a, db := newRefusalAgent(t, "REFUSED_CANDIDATE", engine)
			messages := refusalTranscript()
			original := pycopy.DeepCopy(messages).([]map[string]any)
			tokens := 120000
			system := "sys"
			returned, _, err := compressContext(context.Background(), a, messages, &system, compressContextOptions{ApproxTokens: &tokens, CommitFence: NewCompressionCommitFence()})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(returned, original) || !reflect.DeepEqual(messages, original) {
				t.Fatalf("returned equal=%v live equal=%v", reflect.DeepEqual(returned, original), reflect.DeepEqual(messages, original))
			}
			assertSessionUntouched(t, a, db, "REFUSED_CANDIDATE")
			got := records()
			if len(got) != 1 {
				t.Fatalf("%d telemetry records", len(got))
			}
			if status, _ := got[0].Get("commit_status"); status != "aborted" {
				t.Fatalf("commit_status = %v", status)
			}
			for _, k := range []string{"messages_after", "tokens_after", "tokens_reclaimed"} {
				if v, _ := got[0].Get(k); v != nil {
					t.Fatalf("%s = %v", k, v)
				}
			}
		})
	}
}

func TestRaisingMutatingEngineRestoresOriginalTranscript(t *testing.T) {
	a, db := newRefusalAgent(t, "RAISING_ENGINE", func(_ *ContextCompressor, msgs []map[string]any) ([]map[string]any, error) {
		clearInPlace(msgs)
		return nil, errors.New("context engine failed")
	})
	messages := refusalTranscript()
	original := pycopy.DeepCopy(messages).([]map[string]any)
	tokens := 120000
	system := "sys"
	_, _, err := compressContext(context.Background(), a, messages, &system, compressContextOptions{ApproxTokens: &tokens, CommitFence: NewCompressionCommitFence()})
	if err == nil || !strings.Contains(err.Error(), "context engine failed") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(messages, original) {
		t.Fatal("the raising engine left the live list mutated")
	}
	assertSessionUntouched(t, a, db, "RAISING_ENGINE")
}
