package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestANewLaunchDeletesTheUnfinishedChatAndStartsOverAtTheNameBeat(t *testing.T) {
	h := onboardingSessionRoot(t)
	prevFacts := tools.ReadOnboardingFacts
	tools.ReadOnboardingFacts = func(*tools.Env, string, string) tools.OnboardingFacts {
		return tools.OnboardingFacts{OS: "macos", Arch: "arm64", MachineKind: "Mac"}
	}
	t.Cleanup(func() { tools.ReadOnboardingFacts = prevFacts })
	client, ctx := onboardingSessionClient(t)
	ended := onboardingEndedLog(t)

	first := onbResult(t, ctx, "onboarding.start", map[string]any{"fresh_launch": true})
	old, _ := first["stored_session_id"].(string)
	db := getDB(ctx)
	oldSession, err := db.GetSession(context.Background(), old)
	if err != nil || oldSession == nil {
		t.Fatalf("old session %v %v", oldSession, err)
	}
	oldTitle, _ := oldSession.Get("title")
	if _, err := tools.AdvanceOnboarding(context.Background(), db, old, tools.OnbShown{Beat: "name"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.AdvanceOnboarding(context.Background(), db, old, tools.OnbAnswered{Beat: "name", Answer: map[string]any{"name": "Sid"}}); err != nil {
		t.Fatal(err)
	}
	beats, _ := tools.LoadOnboardingRecord(db, old)["beats"].(map[string]any)
	if beats["name"] == "pending" {
		t.Fatal("the name beat stayed pending after its answer")
	}

	again := onbResult(t, ctx, "onboarding.start", map[string]any{"fresh_launch": true})
	fresh, _ := again["stored_session_id"].(string)
	if fresh == old || again["resumed"] != false || again["run_id"] == first["run_id"] {
		t.Fatalf("first %v again %v", first, again)
	}
	if gone, _ := db.GetSession(context.Background(), old); gone != nil {
		t.Fatal("the unfinished chat survived the new launch")
	}
	if told := ended.snapshot(); len(told) != 1 || told[0]["stored_session_id"] != old || told[0]["outcome"] != "deleted" {
		t.Fatalf("ended %v", told)
	}
	record := tools.LoadOnboardingRecord(db, fresh)
	beats, _ = record["beats"].(map[string]any)
	if tools.ActiveOnboardingSessionID(db) != fresh || beats["name"] != "pending" {
		t.Fatalf("active %q beats %v", tools.ActiveOnboardingSessionID(db), beats)
	}
	var kinds []string
	for _, call := range client.snapshot() {
		kinds = append(kinds, call.kind)
	}
	if !reflect.DeepEqual(kinds, []string{"resume", "submit", "resume", "submit"}) {
		t.Fatalf("client calls %v", kinds)
	}
	freshSession, _ := db.GetSession(context.Background(), fresh)
	if title, _ := freshSession.Get("title"); title != oldTitle {
		t.Fatalf("title %v want %v", title, oldTitle)
	}
	if got := onboardingRunFlag(t, h); got != true {
		t.Fatalf("flag %v", got)
	}
}
