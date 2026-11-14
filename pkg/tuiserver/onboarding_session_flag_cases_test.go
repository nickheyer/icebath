package tuiserver

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestASkipRetriedAfterAFailedConfigWriteClosesTheInstallFlag(t *testing.T) {
	h := onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "onb")
	real := tools.SetOnboardingRun
	tools.SetOnboardingRun = func(context.Context, bool, bool) error { return errors.New("disk full") }
	_, err := tools.AdvanceOnboarding(ctx, db, "onb", tools.OnbEnd{Outcome: "skipped"})
	tools.SetOnboardingRun = real
	if err == nil {
		t.Fatal("the failed flag write was swallowed")
	}
	if got := onbPhase(db, "onb"); got != "skipped" {
		t.Fatalf("phase %v", got)
	}
	if got := onboardingRunFlag(t, h); got != true {
		t.Fatalf("flag after the failed write %v", got)
	}
	if _, err := tools.AdvanceOnboarding(ctx, db, "onb", tools.OnbEnd{Outcome: "skipped"}); err != nil {
		t.Fatal(err)
	}
	if got := onboardingRunFlag(t, h); got != false {
		t.Fatalf("flag after the retry %v", got)
	}
}

func TestAnOldEndedChatNeverClosesTheFlagOfANewerRun(t *testing.T) {
	h := onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "old")
	if _, err := tools.AdvanceOnboarding(ctx, db, "old", tools.OnbEnd{Outcome: "skipped"}); err != nil {
		t.Fatal(err)
	}
	onbStartRecord(t, db, "new")
	if _, err := tools.AdvanceOnboarding(ctx, db, "old", tools.OnbEnd{Outcome: "skipped"}); err != nil {
		t.Fatal(err)
	}
	if got := onboardingRunFlag(t, h); got != true {
		t.Fatalf("flag %v", got)
	}
	if got := tools.ActiveOnboardingSessionID(db); got != "new" {
		t.Fatalf("active %q", got)
	}
}
