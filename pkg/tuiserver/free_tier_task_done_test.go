package tuiserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Clean free tier result the disqualification cases each spoil one way
func cleanTaskResult() map[string]any {
	return map[string]any{"completed": true, "final_response": "Here is your plan.", "user_intervened": false}
}

// Fresh profile home of one case
func freeTierHome(t *testing.T) context.Context {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	return home.WithOverride(context.Background(), h)
}

// Puts the home on the guest welcome trial after its agent is built, the state free inference reads as active
func armFreeTier(t *testing.T, ctx context.Context) {
	t.Helper()
	env := scopedPoolEnv(ctx)
	if err := env.Store.Update(func(data provider.AuthData) (bool, error) {
		providers, _ := data["providers"].(map[string]any)
		if providers == nil {
			providers = map[string]any{}
			data["providers"] = providers
		}
		providers["nous"] = map[string]any{"auth_method": provider.AnonAuthMethod, provider.WelcomeClaimFlag: true}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Agent whose credential is the guest token when anonymous, an own provider key otherwise
func freeTierAgent(t *testing.T, anonymous bool) *agent.AIAgent {
	if !anonymous {
		return testAgent(t, agent.AIAgentOptions{Provider: "openrouter", Model: "test/model"})
	}
	return testAgent(t, agent.AIAgentOptions{Provider: "nous", Model: "test/model", APIKey: freeTierAgentKey()})
}

// Guest bearer whose claims carry the anonymous account tier
func freeTierAgentKey() string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"account_tier":"`+provider.AnonAccountTier+`"}`)) + ".sig"
}

// Live session record shaped like the turn's
func freeTierSession(a *agent.AIAgent, extra map[string]any) *tuigateway.Session {
	fields := map[string]any{"agent": a, "session_key": "k", "history": []any{}, "history_lock": &sync.Mutex{}, "running": true, "attached_images": []string{}}
	for k, v := range extra {
		fields[k] = v
	}
	return tuigateway.NewSession(fields)
}

// Whether the sign in offer clock was started
func offerRecorded(t *testing.T, ctx context.Context) (int, bool) {
	t.Helper()
	due, pending, err := provider.OfferDueIn(scopedPoolEnv(ctx), provider.FreeTierOfferTrial)
	if err != nil {
		t.Fatal(err)
	}
	return due, pending
}

func TestACleanTaskArmsTheOffer(t *testing.T) {
	ctx := freeTierHome(t)
	a := freeTierAgent(t, true)
	armFreeTier(t, ctx)
	session := freeTierSession(a, nil)
	noteTaskDone(ctx, session, cleanTaskResult(), a, "")
	due, pending := offerRecorded(t, ctx)
	if !pending || due != provider.OfferDelaySeconds {
		t.Fatalf("offer due = %d pending = %v, want %d", due, pending, provider.OfferDelaySeconds)
	}
}

func TestDisqualifiedTurnsRecordNothing(t *testing.T) {
	spoil := func(key string, value any) map[string]any {
		r := cleanTaskResult()
		r[key] = value
		return r
	}
	for name, c := range map[string]struct {
		result      map[string]any
		anonymous   bool
		displayKind string
		userInput   bool
	}{
		"not-completed":          {spoil("completed", false), true, "", false},
		"empty-reply":            {spoil("final_response", "  "), true, "", false},
		"pending-steer":          {spoil("pending_steer", "and the weather"), true, "", false},
		"steered-or-interrupted": {spoil("user_intervened", true), true, "", false},
		"typed-while-busy":       {cleanTaskResult(), true, "", true},
		"own-provider":           {cleanTaskResult(), false, "", false},
		"crash-resume":           {cleanTaskResult(), true, "auto_continue", false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := freeTierHome(t)
			a := freeTierAgent(t, c.anonymous)
			armFreeTier(t, ctx)
			extra := map[string]any{}
			if c.userInput {
				extra["_turn_user_input"] = true
			}
			session := freeTierSession(a, extra)
			noteTaskDone(ctx, session, c.result, a, c.displayKind)
			if _, pending := offerRecorded(t, ctx); pending {
				t.Fatal("a disqualified turn armed the offer")
			}
		})
	}
}

// Transport recording every frame and running a probe on each message.complete before the turn goes on
type completeProbeTransport struct {
	frameRecorder
	onComplete func()
}

func (p *completeProbeTransport) Write(obj *pyjson.Object) bool {
	if params, isObj := obj.GetOr("params", nil).(*pyjson.Object); isObj && params.GetOr("type", nil) == "message.complete" {
		p.onComplete()
	}
	return p.frameRecorder.Write(obj)
}

func TestPromptSubmitRecordsOnlyUntouchedTurns(t *testing.T) {
	for _, c := range []struct {
		name, touch                          string
		recordedTurns, recordsAtEachComplete []int
	}{
		{"untouched", "", []int{1}, []int{1}},
		{"steered-over-rpc", "steer", []int{}, []int{0}},
		{"typed-while-busy", "typed", []int{2}, []int{0, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			runs := 0
			var ctx context.Context
			srv, err := fakellm.NewResponder(func(*fakellm.Request) any {
				mu.Lock()
				runs++
				n := runs
				mu.Unlock()
				if c.touch == "steer" {
					handleRequest(ctx, map[string]any{"id": "s", "method": "session.steer", "params": map[string]any{"session_id": "sid", "text": "and my calendar"}})
				}
				if c.touch == "typed" && n == 1 {
					resp := handleRequest(ctx, map[string]any{"id": "t", "method": "prompt.submit", "params": map[string]any{"session_id": "sid", "text": "and my calendar"}})
					if result, _ := resp["result"].(map[string]any); result["status"] != "queued" {
						t.Errorf("busy prompt.submit answered %v", resp)
					}
				}
				return fakellm.Text{Text: "Here is your plan."}
			}, fakellm.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Stop)
			h := sessionCaseScope(t)
			t.Setenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN", "")
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  api_max_retries: 1\ndisplay:\n  busy_input_mode: queue\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			useSharedDB(t, db)
			t.Cleanup(provider.PinFreeTierOfferClockForTests(5000.0))
			offerPath := filepath.Join(h, "free_tier", "sign_in_offer.json")
			recorded, recordedAtComplete := []int{}, []int{}
			takeRecord := func() {
				raw, err := os.ReadFile(offerPath)
				if err != nil {
					return
				}
				record := map[string]any{}
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Error(err)
					return
				}
				if record["last_task_at"] != 5000.0 {
					return
				}
				mu.Lock()
				recorded = append(recorded, runs)
				mu.Unlock()
				record["last_task_at"] = -1.0
				out, _ := json.Marshal(record)
				if err := os.WriteFile(offerPath, out, 0o600); err != nil {
					t.Error(err)
				}
			}
			transport := &completeProbeTransport{onComplete: func() {
				takeRecord()
				mu.Lock()
				recordedAtComplete = append(recordedAtComplete, len(recorded))
				mu.Unlock()
			}}
			ctx = tuigateway.BindTransport(context.Background(), transport)
			opts := agent.AIAgentOptions{Model: fakellm.ModelID, Provider: "nous", BaseURL: srv.BaseURL(), APIKey: freeTierAgentKey(),
				APIMode: "chat_completions", SessionID: "k", Platform: "tui", QuietMode: true, SkipContextFiles: true, SkipMemory: true,
				MaxIterations: 10, SessionDB: db, EnabledToolsets: []string{"todo"}}
			agentCbs(ctx, "sid").applyTo(&opts)
			built, err := agent.NewAIAgent(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			armFreeTier(t, home.WithOverride(context.Background(), h))
			initSession(ctx, "sid", "k", built, []any{}, 80, h, nil, "", "", false)
			session := tuigateway.SessionRecord("sid")
			t.Cleanup(func() { closeSessionByID(context.Background(), "sid", "test_cleanup", nil) })
			resp := handleRequest(ctx, map[string]any{"id": "1", "method": "prompt.submit", "params": map[string]any{"session_id": "sid", "text": "Plan my week"}})
			if _, answered := resp["result"]; !answered {
				t.Fatalf("prompt.submit answered %v", resp)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				mu.Lock()
				completes := len(recordedAtComplete)
				mu.Unlock()
				if completes >= len(c.recordsAtEachComplete) && !truthy(session.Get("running")) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("turns never settled, %d message.complete", completes)
				}
				time.Sleep(10 * time.Millisecond)
			}
			takeRecord()
			if !reflect.DeepEqual(recorded, c.recordedTurns) {
				t.Fatalf("recorded = %v want %v", recorded, c.recordedTurns)
			}
			if !reflect.DeepEqual(recordedAtComplete, c.recordsAtEachComplete) {
				t.Fatalf("recorded at complete = %v want %v", recordedAtComplete, c.recordsAtEachComplete)
			}
			if _, leaked := session.Lookup("_turn_user_input"); leaked {
				t.Fatal("_turn_user_input leaked into the next turn")
			}
		})
	}
}
