package provider

import (
	"strings"
	"testing"
	"time"
)

func plBool(b bool) *bool { return &b }

func TestPlanLimitCreditsOption(t *testing.T) {
	s := NewCreditsState()
	s.PurchasedMicros, s.KeepGoingOnCredits = 5_000_000, plBool(false)
	if CreditsOptionFor(s).Action != UseCredits {
		t.Fatal("balance with keep going off offers use credits")
	}
	for _, c := range []struct {
		purchased int64
		keep      *bool
	}{{5_000_000, plBool(true)}, {0, nil}} {
		s.PurchasedMicros, s.KeepGoingOnCredits = c.purchased, c.keep
		if CreditsOptionFor(s).Action != AddCredits {
			t.Fatal("no balance or setting on offers add credits")
		}
	}
	s.PurchasedMicros, s.KeepGoingOnCredits = 0, plBool(false)
	if CreditsOptionFor(s).Action != AddCreditsAndKeepGoing || CreditsAction(plBool(false), 0) != AddCreditsAndKeepGoing {
		t.Fatal("keep going off with no balance needs both steps")
	}
	s.PausedReason, s.PurchasedMicros = "payment_past_due", 5_000_000
	if CreditsOptionFor(s) != nil || CreditsOptionFor(nil) != nil {
		t.Fatal("paused offers nothing")
	}
}

func plPayload(over map[string]any) map[string]any {
	base := map[string]any{"gauge_id": "day", "scope": "all", "limit_name": "daily", "resets_at_ms": int64(1_900_000_000_000), "message": "m",
		"suggestions": []any{map[string]any{"model": "example/model-free:free", "label": "Model Free", "note": "free", "provider": "nous"}},
		"credits":     map[string]any{"action": "add_credits"}}
	for k, v := range over {
		base[k] = v
	}
	return base
}

func TestPlanLimitNextSteps(t *testing.T) {
	if got := LimitNextStepsFromPayload(plPayload(nil), "/model", "/topup", false, false); got != "Switch to Model Free (free) with /model, or add credits with /topup." {
		t.Fatalf("terminal copy %q", got)
	}
	if got := LimitNextStepsFromPayload(plPayload(map[string]any{"credits": map[string]any{"action": "use_credits"}}), "/model", "/topup", false, false); !strings.HasSuffix(got, "or turn on Keep going in /topup to use credits.") {
		t.Fatalf("use credits %q", got)
	}
	both := plPayload(map[string]any{"credits": map[string]any{"action": "add_credits_and_keep_going"}})
	if !strings.HasSuffix(LimitNextStepsFromPayload(both, "/model", "/topup", false, false), "or add credits and turn on Keep going in /topup.") ||
		!strings.HasSuffix(LimitNextStepsFromPayload(both, "/model", "", true, false), "or add credits and turn on Keep going in the Portal.") {
		t.Fatal("both steps")
	}
	got := LimitNextStepsFromPayload(plPayload(nil), "!model", "", true, false)
	if got != "Switch to Model Free (free) with !model, or add credits in the Portal." || strings.Contains(got, "/topup") {
		t.Fatalf("messaging copy %q", got)
	}
	if got = LimitNextStepsFromPayload(plPayload(map[string]any{"suggestions": []any{}, "credits": nil}), "/model", "/topup", false, false); got != "Pick another model with /model." {
		t.Fatalf("no suggestion %q", got)
	}
	if got = LimitNextStepsFromPayload(plPayload(nil), "/model", "/topup", false, true); !strings.Contains(got, "[bold]/model[/]") || !strings.Contains(got, "[bold]/topup[/]") {
		t.Fatalf("markup %q", got)
	}
	if got = LimitNextStepsFromPayload(map[string]any{"suggestions": []any{map[string]any{"label": "no model"}, "junk"}, "credits": "junk"}, "/model", "/topup", false, false); got != "Pick another model with /model." {
		t.Fatalf("malformed payload %q", got)
	}
}

func TestPlanLimitChatRefusalPortalWording(t *testing.T) {
	if got := ChatRefusalWording("Limit · /topup to add credits", 0); got != "Limit · add credits in the Portal" {
		t.Fatalf("portal wording %q", got)
	}
}

func TestPlanLimitChatRefusalResetDuration(t *testing.T) {
	resetsAt := float64(time.Now().UnixNano())/1e9 + 3*3600 + 30
	text := "You've hit your daily limit · resets " + FormatWindowReset(int64(resetsAt*1000), 0) + " · /topup to add credits"
	if got := ChatRefusalWording(text, resetsAt); got != "You've hit your daily limit · resets in 3h · add credits in the Portal" {
		t.Fatalf("chat refusal %q", got)
	}
}
