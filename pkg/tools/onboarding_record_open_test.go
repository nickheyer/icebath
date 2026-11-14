package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Opens a state DB under a temporary home with one onboarding chat started
func onboardingRecordDB(t *testing.T, sessions ...string) *state.DB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.Open(home.StatePath(h), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, sid := range sessions {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestActiveOnboardingSessionIDFollowsTheOpenRun(t *testing.T) {
	db := onboardingRecordDB(t, "onb", "plain")
	if got := ActiveOnboardingSessionID(db); got != "" {
		t.Fatalf("active before start = %q", got)
	}
	if _, err := StartOnboarding(context.Background(), db, "onb", OnboardingFacts{OS: "linux"}); err != nil {
		t.Fatal(err)
	}
	if got := OpenOnboardingSessionIDs(db); len(got) != 1 || got[0] != "onb" {
		t.Fatalf("open = %v", got)
	}
	if got := ActiveOnboardingSessionID(db); got != "onb" {
		t.Fatalf("active = %q", got)
	}
}

func TestObserveOnboardingToolResultPassesThroughOutsideAnOnboardingChat(t *testing.T) {
	db := onboardingRecordDB(t, "onb")
	if _, err := StartOnboarding(context.Background(), db, "onb", OnboardingFacts{OS: "linux"}); err != nil {
		t.Fatal(err)
	}
	result := `{"success": true}`
	if got := ObserveOnboardingToolResult(context.Background(), db, "onb", true, "terminal", nil, result); got != result {
		t.Fatalf("unrelated tool = %s", got)
	}
	if got := ObserveOnboardingToolResult(context.Background(), db, "onb", false, "set_theme", nil, result); got != result {
		t.Fatalf("no setup beat = %s", got)
	}
	if got := ObserveOnboardingToolResult(context.Background(), db, "other", true, "set_theme", nil, result); got != result {
		t.Fatalf("chat without a run = %s", got)
	}
	if got := ObserveOnboardingToolResult(context.Background(), db, "onb", true, "set_theme", nil, "not json"); got != "not json" {
		t.Fatalf("non JSON result = %s", got)
	}
}

func TestObserveOnboardingToolResultKeepsTheResultKeyOrder(t *testing.T) {
	db := onboardingRecordDB(t, "onb")
	if _, err := StartOnboarding(context.Background(), db, "onb", OnboardingFacts{OS: "linux"}); err != nil {
		t.Fatal(err)
	}
	result := `{"success": true, "applied": {"layout": "terminal-deck"}}`
	got := ObserveOnboardingToolResult(context.Background(), db, "onb", true, "set_theme", map[string]any{"layout": "terminal-deck"}, result)
	if !strings.HasPrefix(got, `{"success": true, "applied": {"layout": "terminal-deck"}, "reminder": `) {
		t.Fatalf("result = %s", got)
	}
}

func TestObserveOnboardingToolResultRedumpsNumbersAsPythonDoes(t *testing.T) {
	db := onboardingRecordDB(t, "onb")
	if _, err := StartOnboarding(context.Background(), db, "onb", OnboardingFacts{OS: "linux"}); err != nil {
		t.Fatal(err)
	}
	result := `{"success": true, "applied": {"layout": "terminal-deck"}, "n": 1, "r": 1.50, "e": 1e5, "f": 1.0}`
	got := ObserveOnboardingToolResult(context.Background(), db, "onb", true, "set_theme", map[string]any{"layout": "terminal-deck"}, result)
	if !strings.HasPrefix(got, `{"success": true, "applied": {"layout": "terminal-deck"}, "n": 1, "r": 1.5, "e": 100000.0, "f": 1.0, "reminder": `) {
		t.Fatalf("result = %s", got)
	}
}
