package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const switchSessionID = "switch-session"

func switchStoredPrompt(model, prov string) string {
	return "You are Hermes Agent.\n\nConversation started: Thursday, September 24, 2026\nModel: " + model + "\nProvider: " + prov
}

func switchDB(t *testing.T, prompt string) *state.SessionDB {
	t.Helper()
	db := restoreRealDB(t, switchSessionID, "discord")
	if err := db.UpdateSystemPrompt(context.Background(), switchSessionID, &prompt); err != nil {
		t.Fatal(err)
	}
	return db
}

func switchStored(t *testing.T, db *state.SessionDB) string {
	t.Helper()
	row, err := db.GetSession(context.Background(), switchSessionID)
	if err != nil || row == nil {
		t.Fatalf("session row %v %v", row, err)
	}
	prompt, _ := row.GetOr("system_prompt", "").(string)
	return prompt
}

// One restore on a fresh per turn agent whose rebuild renders the real trailer, returning agent and build count
func switchRunTurn(t *testing.T, db *state.SessionDB, model, prov, prose string) (*AIAgent, int) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	builds := 0
	prev := loopBuildSystemPrompt
	loopBuildSystemPrompt = func(ctx context.Context, a *AIAgent, _ *string) (string, error) {
		builds++
		return "You are Hermes Agent.\n\n" + prose + timestampLine(ctx, a), nil
	}
	defer func() { loopBuildSystemPrompt = prev }()
	a := &AIAgent{sessionID: switchSessionID, model: model, provider: prov, platform: "discord", sessionDB: db,
		env: &tools.Env{Home: h, Cwd: h}, quietMode: true, sessionPersistLock: &pythread.RLock{}}
	if err := restoreOrBuildSystemPrompt(tools.WithToolEnv(context.Background(), a.env), a, nil, hiHistory()); err != nil {
		t.Fatal(err)
	}
	return a, builds
}

func TestEmptiedLiveIdentityRebuildsOnceThenReuses(t *testing.T) {
	for _, tc := range []struct{ id, model, prov string }{{"provider_empty", "x-ai/grok-4.5", ""}, {"model_empty", "", "nous"}} {
		t.Run(tc.id, func(t *testing.T) {
			prose := "MEMORY\nProvider: openrouter\nModel: some/other-model\n\n"
			stale := switchStoredPrompt("x-ai/grok-4.5", "nous")
			db := switchDB(t, stale)
			if _, builds := switchRunTurn(t, db, tc.model, tc.prov, prose); builds != 1 {
				t.Fatalf("first turn builds %d", builds)
			}
			rebuilt := switchStored(t, db)
			if rebuilt == stale {
				t.Fatal("stale prompt kept")
			}
			second, builds := switchRunTurn(t, db, tc.model, tc.prov, prose)
			if builds != 0 || second.cachedSystemPrompt != rebuilt {
				t.Fatalf("second turn builds %d cached %q stored %q", builds, second.cachedSystemPrompt, rebuilt)
			}
		})
	}
}

func TestPromptWithoutIdentityLinesKeepsReusing(t *testing.T) {
	legacy := "You are Hermes Agent.\n\nConversation started: Thursday, September 24, 2026"
	db := switchDB(t, legacy)
	a, builds := switchRunTurn(t, db, "x-ai/grok-4.5", "", "")
	if builds != 0 || a.cachedSystemPrompt != legacy {
		t.Fatalf("builds %d cached %q", builds, a.cachedSystemPrompt)
	}
}
