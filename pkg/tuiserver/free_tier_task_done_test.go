package tuiserver

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Clean free tier result the disqualification cases each spoil one way
func cleanTaskResult() map[string]any {
	return map[string]any{"completed": true, "final_response": "Here is your plan.", "user_intervened": false}
}

// Profile home on the guest welcome trial, the state free inference reads as active
func freeTierHome(t *testing.T) context.Context {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	ctx := home.WithOverride(context.Background(), h)
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
	return ctx
}

// Agent whose credential is the guest token when anonymous, an own provider key otherwise
func freeTierAgent(t *testing.T, anonymous bool) *agent.AIAgent {
	if !anonymous {
		return testAgent(t, agent.AIAgentOptions{Provider: "openrouter", Model: "test/model"})
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"account_tier":"` + provider.AnonAccountTier + `"}`))
	return testAgent(t, agent.AIAgentOptions{Provider: "nous", Model: "test/model", APIKey: "e30." + payload + ".sig"})
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
