package tuiserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Frames of the recorder decoded into plain values
func approvalFrames(r *frameRecorder) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.frames))
	for _, f := range r.frames {
		out = append(out, pyjson.Plain(f).(map[string]any))
	}
	return out
}

// Agent built on an env the build bound then cleared, so the turn must bind it again
func TestADangerousCommandOfATurnAsksTheClientAndADenyBlocksIt(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	srv, err := fakellm.New([]any{fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "rm -rf " + victim}}, fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	h := sessionCaseScope(t)
	t.Setenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN", "")
	for _, name := range []string{"ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK", "ICEBATH_INTERACTIVE"} {
		t.Setenv(name, "1")
	}
	if err := fakellm.WriteHome(h, srv.BaseURL(), "", "approvals:\n  mode: manual\n"); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	useSharedDB(t, db)
	recorder := &frameRecorder{}
	ctx := tuihost.BindTransport(context.Background(), recorder)
	opts := agent.AIAgentOptions{Model: fakellm.ModelID, Provider: "custom", BaseURL: srv.BaseURL(), APIKey: "sk-fake-e2e",
		APIMode: "chat_completions", SessionID: "approval-key", Platform: "tui", QuietMode: true, SkipContextFiles: true, SkipMemory: true,
		MaxIterations: new(10), SessionDB: db, EnabledToolsets: []string{"terminal"}}
	agentCbs(ctx, "approval-sid").applyTo(&opts)
	buildCtx, buildEnv := setSessionContext(ctx, "approval-key", nil, "approval-sid")
	built, err := agent.NewAIAgent(buildCtx, opts)
	clearSessionContext(buildEnv)
	if err != nil {
		t.Fatal(err)
	}
	initSession(ctx, "approval-sid", "approval-key", built, []any{}, 80, h, nil, "", "", false)
	t.Cleanup(func() { closeSessionByID(context.Background(), "approval-sid", "test_cleanup", nil) })
	resp := handleRequest(ctx, map[string]any{"id": "turn", "method": "prompt.submit", "params": map[string]any{"session_id": "approval-sid", "text": "go"}})
	if result, _ := resp["result"].(map[string]any); result["status"] != "streaming" {
		t.Fatalf("prompt.submit answered %v", resp)
	}
	var request map[string]any
	for deadline := time.Now().Add(20 * time.Second); request == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, frame := range approvalFrames(recorder) {
			if frame["method"] == "approval" {
				request = frame
			}
		}
	}
	params, _ := request["params"].(map[string]any)
	if request == nil || !strings.Contains(strOr(params["command"]), victim) {
		t.Fatalf("no approval request reached the client, frames %v", approvalFrames(recorder))
	}
	if resolved := tools.ResolveGatewayApproval("approval-key", "deny", true, "", ""); resolved != 1 {
		t.Fatalf("resolved %d", resolved)
	}
	var outcome map[string]any
	for deadline := time.Now().Add(20 * time.Second); outcome == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, frame := range approvalFrames(recorder) {
			if p, _ := frame["params"].(map[string]any); frame["method"] == "event" && p["type"] == "tool.complete" {
				payload, _ := p["payload"].(map[string]any)
				if text, isText := payload["result"].(string); isText {
					decoded, _ := pyjson.Loads(text)
					outcome, _ = pyjson.Plain(decoded).(map[string]any)
				}
			}
		}
	}
	if outcome == nil || outcome["status"] != "blocked" {
		t.Fatalf("tool outcome %v", outcome)
	}
}
