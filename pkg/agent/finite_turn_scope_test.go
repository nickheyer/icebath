package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

type finiteObservation struct {
	finite, known, unattended, noUserCanAnswer bool
}

// Agent whose fake model records the finite flags the tools read while the turn runs
func finiteScopeAgent(t *testing.T) (cron.CronAgent, *tools.Env, func() []finiteObservation) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	var mu sync.Mutex
	var seen []finiteObservation
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test-model", "object": "model"}}})
			return
		}
		_, _ = io.ReadAll(r.Body)
		finite, known := tools.FiniteTurnFlag(env)
		mu.Lock()
		seen = append(seen, finiteObservation{finite, known, tools.UnattendedTurnFlag(env), tools.NoUserCanAnswer(env)})
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-finite", "object": "chat.completion", "model": "test-model", "usage": nil,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done", "tool_calls": []any{}}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	built, err := newCronAgent(tools.WithToolEnv(context.Background(), env), cron.AgentOptions{
		Model: "test-model", Runtime: &provider.Runtime{APIKey: "test-key", BaseURL: srv.URL + "/v1"}, MaxIterations: 3,
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, SkipBackgroundReview: true, Platform: "cron", SessionID: "finite_probe", Env: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { built.Close() })
	built.(cronAgent).DisableStreaming()
	return built, env, func() []finiteObservation {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
}

// A turn under an admission's finite scope shows its flags to the tools and leaves the env as it found it
func TestAdmittedFiniteTurnBindsTheToolFlagsForTheTurnOnly(t *testing.T) {
	built, env, observed := finiteScopeAgent(t)
	base := tools.WithToolEnv(context.Background(), env)
	for _, tc := range []struct {
		name               string
		ctx                context.Context
		want               finiteObservation
		unattendedApproves bool
	}{
		{"oneshot", gateway.FiniteTurnScope(base, true, true), finiteObservation{true, true, true, true}, true},
		{"card", gateway.FiniteTurnScope(base, true, false), finiteObservation{true, true, false, true}, false},
		{"interactive", gateway.FiniteTurnScope(base, false, true), finiteObservation{false, true, false, false}, false},
		{"outside an admission", base, finiteObservation{false, false, false, false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed()
			if _, err := built.RunConversation(tc.ctx, "go", "task-"+tc.name); err != nil {
				t.Fatal(err)
			}
			got := observed()
			if len(got) == 0 || got[len(got)-1] != tc.want {
				t.Fatalf("flags during the turn %+v, want %+v", got, tc.want)
			}
			if gateway.UnattendedTurn(tc.ctx) != tc.unattendedApproves {
				t.Fatalf("unattended %v", gateway.UnattendedTurn(tc.ctx))
			}
			if _, known := tools.FiniteTurnFlag(env); known || tools.UnattendedTurnFlag(env) {
				t.Fatal("the turn left its finite flags on the env")
			}
		})
	}
	tools.SetFiniteTurn(env, true)
	if _, err := built.RunConversation(gateway.FiniteTurnScope(base, false, false), "go", "task-cli"); err != nil {
		t.Fatal(err)
	}
	if finite, known := tools.FiniteTurnFlag(env); !finite || !known {
		t.Fatal("the launch finite flag was not restored after an admitted turn")
	}
}
