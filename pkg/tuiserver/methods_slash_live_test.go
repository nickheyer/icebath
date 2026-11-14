package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Local session record with a short user and assistant history and the given agent
func liveSlashSession(a *agent.AIAgent) *tuigateway.Session {
	fields := map[string]any{"session_key": "sk-1", "session_id": "sid-1", "running": false, "history_lock": &sync.Mutex{},
		"history": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": "hi"}}}
	if a != nil {
		fields["agent"] = a
	}
	return tuigateway.NewSession(fields)
}

// Live agent on a dead loopback endpoint for the context view
func liveSlashAgent(t *testing.T) *agent.AIAgent {
	return testAgent(t, agent.AIAgentOptions{Model: "test-model", Provider: "openrouter"})
}

func TestContextLocalSessionWithLiveAgent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, ok := liveSlashCommandOutput(context.Background(), "sid-1", liveSlashSession(liveSlashAgent(t)), "context", "")
	if !ok {
		t.Fatal("/context fell through to the CLI worker for a local session")
	}
	if strings.Contains(out, "No active agent") || !strings.Contains(out, "Conversation:") {
		t.Fatalf("out = %q", out)
	}
}

func TestContextLocalSessionAgentless(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, ok := liveSlashCommandOutput(context.Background(), "sid-1", liveSlashSession(nil), "context", "")
	if !ok || strings.Contains(out, "No active agent") || !strings.Contains(out, "Conversation:") {
		t.Fatalf("out = %q ok = %v", out, ok)
	}
}

func TestContextComputeHostRouteUnchanged(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("dashboard:\n  turn_isolation: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := liveSlashSession(liveSlashAgent(t))
	session.Set("_compute_host_active", true)
	if !sessionUsesComputeHost(context.Background(), session, nil) {
		t.Fatal("session does not route to the compute host")
	}
	out, ok := liveSlashCommandOutput(context.Background(), "sid-1", session, "context", "")
	if !ok || !strings.Contains(out, "Conversation:") {
		t.Fatalf("out = %q ok = %v", out, ok)
	}
}

func TestToolsLocalStillFallsThrough(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if out, ok := liveSlashCommandOutput(context.Background(), "sid-1", liveSlashSession(nil), "tools", ""); ok {
		t.Fatalf("tools answered locally: %q", out)
	}
}

// Swaps the breakdown and details engines for the duration of a case
func stubContextEngines(t *testing.T, payload agent.ContextBreakdown, details *agent.ContextDetails) (*int, *int) {
	breakdownCalls, detailsCalls := new(int), new(int)
	prevBreakdown, prevDetails := computeSessionContextBreakdown, computeContextDetails
	computeSessionContextBreakdown = func(context.Context, *agent.AIAgent, []map[string]any) (agent.ContextBreakdown, error) {
		*breakdownCalls++
		return payload, nil
	}
	computeContextDetails = func(context.Context, *agent.AIAgent) (agent.ContextDetails, error) {
		*detailsCalls++
		if details == nil {
			return agent.ContextDetails{}, nil
		}
		return *details, nil
	}
	t.Cleanup(func() { computeSessionContextBreakdown, computeContextDetails = prevBreakdown, prevDetails })
	return breakdownCalls, detailsCalls
}

func TestContextBreakdownEngineUsedWithLiveAgent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stubContextEngines(t, agent.ContextBreakdown{
		Categories: []agent.ContextCategory{{ID: "system_prompt", Label: "System prompt", Tokens: 8000, Color: "dim"},
			{ID: "conversation", Label: "Conversation", Tokens: 82000, Color: "normal"}},
		ContextMax: 131072, ContextPercent: 68, ContextUsed: 90000, ContextSource: "provider_usage", EstimatedTotal: 90000, Model: "test-model",
	}, nil)
	out, ok := liveSlashCommandOutput(context.Background(), "sid-1", liveSlashSession(liveSlashAgent(t)), "context", "")
	if !ok || !strings.Contains(out, "Context window:") || !strings.Contains(out, "90,000") {
		t.Fatalf("out = %q ok = %v", out, ok)
	}
}

func TestContextAllArgReachesDetails(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	breakdownCalls, detailsCalls := stubContextEngines(t, agent.ContextBreakdown{
		Categories: []agent.ContextCategory{{ID: "conversation", Label: "Conversation", Tokens: 1000, Color: "normal"}},
		ContextMax: 10000, ContextPercent: 10, ContextUsed: 1000, ContextSource: "provider_usage", EstimatedTotal: 1000, Model: "test-model",
	}, &agent.ContextDetails{})
	if _, ok := liveSlashCommandOutput(context.Background(), "sid-1", liveSlashSession(liveSlashAgent(t)), "context", "all"); !ok {
		t.Fatal("context all fell through")
	}
	if *breakdownCalls != 1 || *detailsCalls != 1 {
		t.Fatalf("breakdown calls %d details calls %d", *breakdownCalls, *detailsCalls)
	}
}

// Agent double recording each explicit background review it is asked to spawn
type refineRecordingAgent struct{ spawned []map[string]any }

func (a *refineRecordingAgent) ValidToolNames() map[string]bool {
	return map[string]bool{"memory": true, "skill_manage": true}
}

func (a *refineRecordingAgent) SpawnExplicitBackgroundReview(ctx context.Context, snapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string) {
	var f any
	if focus != nil {
		f = *focus
	}
	a.spawned = append(a.spawned, map[string]any{"messages_snapshot": snapshot, "review_memory": reviewMemory, "review_skills": reviewSkills,
		"focus": f, "explicit": true})
}

// Session record of the refine cases holding the recording agent
func refineSession(a *refineRecordingAgent, key string, history []any) *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"agent": a, "session_key": key, "history": history, "history_lock": &sync.Mutex{},
		"running": false, "cwd": "", "source": "desktop"})
}

// Spawned review of the recording agent with only role and content kept on each snapshot row
func refineSpawned(a *refineRecordingAgent) []map[string]any {
	out := make([]map[string]any, 0, len(a.spawned))
	for _, call := range a.spawned {
		rows := []map[string]any{}
		for _, row := range call["messages_snapshot"].([]map[string]any) {
			rows = append(rows, map[string]any{"role": row["role"], "content": row["content"]})
		}
		copied := map[string]any{}
		for k, v := range call {
			copied[k] = v
		}
		copied["messages_snapshot"] = rows
		out = append(out, copied)
	}
	return out
}

func TestRefineSpawnsReviewFromPersistedTranscript(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "refine-key", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "persisted question"}, {"role": "assistant", "content": "persisted answer"}} {
		if _, err := db.AppendMessage(ctx, "refine-key", m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	a := &refineRecordingAgent{}
	session := refineSession(a, "refine-key", []any{})
	session.Set("profile_home", h)
	tuigateway.SetSessionRecord("refine-sid", session)
	t.Cleanup(func() { tuigateway.PopSessionRecord("refine-sid") })
	out, _ := liveSlashCommandOutput(ctx, "refine-sid", session, "refine", "save the workflow")
	if !strings.Contains(out, "Reviewing this conversation") {
		t.Fatalf("out = %q", out)
	}
	want := []map[string]any{{"messages_snapshot": []map[string]any{{"role": "user", "content": "persisted question"},
		{"role": "assistant", "content": "persisted answer"}}, "review_memory": true, "review_skills": true, "focus": "save the workflow", "explicit": true}}
	if got := refineSpawned(a); !reflect.DeepEqual(got, want) {
		t.Fatalf("spawned = %#v", got)
	}
}

func TestRefineSpawnsReviewFromInMemoryHistoryMidConversation(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &refineRecordingAgent{}
	history := []any{map[string]any{"role": "user", "content": "in-flight question"}, map[string]any{"role": "assistant", "content": "in-flight answer"}}
	session := refineSession(a, "mid-key", history)
	tuigateway.SetSessionRecord("refine-mid", session)
	t.Cleanup(func() { tuigateway.PopSessionRecord("refine-mid") })
	out, _ := liveSlashCommandOutput(context.Background(), "refine-mid", session, "refine", "")
	if !strings.Contains(out, "Reviewing this conversation") {
		t.Fatalf("out = %q", out)
	}
	want := []map[string]any{{"messages_snapshot": []map[string]any{{"role": "user", "content": "in-flight question"},
		{"role": "assistant", "content": "in-flight answer"}}, "review_memory": true, "review_skills": true, "focus": nil, "explicit": true}}
	if got := refineSpawned(a); !reflect.DeepEqual(got, want) {
		t.Fatalf("spawned = %#v", got)
	}
}
