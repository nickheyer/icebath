package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	refusalNowMs      = int64(1_800_000_000_000)
	refusalDayReset   = refusalNowMs + 6*3600*1000
	refusalWeekReset  = refusalNowMs + 3*24*3600*1000
	refusalMonthReset = refusalNowMs + 20*24*3600*1000
)

type refusalState struct {
	day, week, top  int
	topReset        int64
	purchasedMicros int64
	keepGoing       string
}

func refusalGauges(day, week, top int, topReset int64) []map[string]any {
	if topReset == 0 {
		topReset = refusalDayReset
	}
	return []map[string]any{
		{"id": "day", "scope": "all", "label": "Today", "used_bp": day, "resets_at_ms": refusalDayReset},
		{"id": "week", "scope": "all", "label": "This week", "used_bp": week, "resets_at_ms": refusalWeekReset},
		{"id": "top_tier", "scope": "top_tier", "label": "Large models (monthly)", "used_bp": top, "resets_at_ms": topReset},
	}
}

func refusalAgent(t *testing.T, s refusalState) *AIAgent {
	t.Helper()
	a := &AIAgent{env: &tools.Env{Values: &sync.Map{}}, provider: "nous", model: "example/model-b",
		baseURL: "https://inference-api.nousresearch.com/v1"}
	gauges, err := json.Marshal(refusalGauges(s.day, s.week, s.top, s.topReset))
	if err != nil {
		t.Fatal(err)
	}
	st := provider.NewCreditsState()
	st.Version, st.DenominatorKind, st.PaidAccess, st.PurchasedUSD, st.FromHeader = 1, "usage_window", true, "0.00", true
	st.CapturedAt, st.Gauges, st.PurchasedMicros = float64(time.Now().UnixNano())/1e9, provider.ParseGauges(string(gauges)), s.purchasedMicros
	if s.keepGoing == "off" {
		off := false
		st.KeepGoingOnCredits = &off
	}
	a.creditsCapture().AdoptCreditsStateForTests(st)
	return a
}

func refusalWindow(extra map[string]any) *provider.Classified {
	ctx := map[string]any{"usage_window": true}
	for k, v := range extra {
		ctx[k] = v
	}
	return &provider.Classified{Reason: "billing", Context: ctx}
}

func TestRefusalReadsAsTheStopLine(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, pricing map[string]any, featured []string) {
		caseHome(t)
		if pricing == nil {
			pricing = map[string]any{}
		}
		t.Cleanup(provider.SwapPlanLimitSourcesForTests(pricing, featured, nil))
	}
	full := refusalState{day: 10_000, week: 900}
	dayReset := float64(refusalDayReset) / 1000
	monthReset := float64(refusalMonthReset) / 1000
	resetAt := func(t *testing.T, r UsageWindowRefusal) float64 {
		t.Helper()
		if r.ResetsAt == nil {
			t.Fatalf("no reset in %+v", r)
		}
		return *r.ResetsAt
	}
	t.Run("test_the_refusal_text_is_the_stop_line", func(t *testing.T) {
		setup(t, nil, nil)
		s := full
		s.purchasedMicros, s.keepGoing = 5_000_000, "off"
		r := usageWindowRefusal(ctx, refusalAgent(t, s), refusalWindow(nil), nil)
		want := "You've hit your daily limit · resets " + provider.FormatWindowReset(refusalDayReset, 0) + " · turn on Keep going in /topup"
		if r.Message != want || resetAt(t, r) != dayReset {
			t.Fatalf("text %q resets %v", r.Message, r.ResetsAt)
		}
		if r.Limit["gauge_id"] != "day" || r.Limit["scope"] != "all" || r.Limit["message"] != want {
			t.Fatalf("limit %v", r.Limit)
		}
		if !reflect.DeepEqual(r.Limit["credits"], map[string]string{"action": "use_credits"}) || r.Limit["current_model"] != "example/model-b" {
			t.Fatalf("limit %v", r.Limit)
		}
	})
	t.Run("test_a_regular_model_refused_by_its_day_names_the_day_everywhere", func(t *testing.T) {
		setup(t, map[string]any{"example/model-b": map[string]any{"prompt": "0.0000004", "completion": "0.000004", "usage_level": "medium"}}, nil)
		s := full
		s.top, s.topReset = 10_000, refusalMonthReset
		a := refusalAgent(t, s)
		r := usageWindowRefusal(ctx, a, refusalWindow(nil), nil)
		if !strings.HasPrefix(r.Message, "You've hit your daily limit · resets "+provider.FormatWindowReset(refusalDayReset, 0)) {
			t.Fatalf("text %q", r.Message)
		}
		if resetAt(t, r) != dayReset || r.Limit["gauge_id"] != "day" {
			t.Fatalf("resets %v limit %v", r.ResetsAt, r.Limit)
		}
		if got := usageWindowResetContext(a); !reflect.DeepEqual(got, map[string]any{"reset_at": dayReset}) {
			t.Fatalf("pool reset %v", got)
		}
	})
	t.Run("test_a_model_of_unknown_tier_is_never_promised_the_earlier_reset", func(t *testing.T) {
		setup(t, nil, nil)
		s := full
		s.top, s.topReset = 10_000, refusalMonthReset
		a := refusalAgent(t, s)
		r := usageWindowRefusal(ctx, a, refusalWindow(nil), nil)
		if !strings.HasPrefix(r.Message, "You've hit your monthly large-model limit · resets "+provider.FormatWindowReset(refusalMonthReset, 0)) {
			t.Fatalf("text %q", r.Message)
		}
		if resetAt(t, r) != monthReset || r.Limit["gauge_id"] != "top_tier" {
			t.Fatalf("resets %v limit %v", r.ResetsAt, r.Limit)
		}
		if got := usageWindowResetContext(a); !reflect.DeepEqual(got, map[string]any{"reset_at": monthReset}) {
			t.Fatalf("pool reset %v", got)
		}
	})
	t.Run("test_keep_going_off_with_no_balance_says_both_steps_everywhere", func(t *testing.T) {
		setup(t, nil, nil)
		s := full
		s.keepGoing = "off"
		a := refusalAgent(t, s)
		r := usageWindowRefusal(ctx, a, refusalWindow(nil), nil)
		if !strings.HasSuffix(r.Message, " · add credits and turn on Keep going in /topup") {
			t.Fatalf("text %q", r.Message)
		}
		if !reflect.DeepEqual(r.Limit["credits"], map[string]string{"action": "add_credits_and_keep_going"}) {
			t.Fatalf("limit %v", r.Limit)
		}
		raw, _ := json.Marshal(refusalGauges(100, 50, 0, 0))
		off, paid, zero := false, true, 0.0
		window := &provider.NousUsageWindowInfo{Gauges: provider.ParseGauges(string(raw)), AutoTopUpEnabled: &off, KeepGoingOnCredits: &off}
		for _, purchased := range []*float64{&zero, nil} {
			info := &provider.NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid, UsageWindow: window,
				PaidServiceAccessInfo: &provider.NousPaidServiceAccessInfo{PurchasedCreditsRemaining: purchased}}
			if g := provider.FormatUsageWindowBlockGuidance(info); !strings.HasPrefix(g, "Add credits and turn on Keep going in /topup") {
				t.Fatalf("guidance %q", g)
			}
		}
		other := usageWindowRefusal(ctx, a, &provider.Classified{Reason: "billing", Context: map[string]any{}}, nil)
		if other.Message != "" || other.ResetsAt != nil || other.Limit != nil {
			t.Fatalf("plain billing failure carried a window %+v", other)
		}
	})
	t.Run("test_the_server_wording_replaces_the_stop_line_when_sent", func(t *testing.T) {
		setup(t, nil, nil)
		a := refusalAgent(t, full)
		r := usageWindowRefusal(ctx, a, refusalWindow(map[string]any{"display_message": "  Daily limit hit. Back at 4pm.  "}), nil)
		if r.Message != "Daily limit hit. Back at 4pm." || resetAt(t, r) != dayReset || r.Limit["message"] != "Daily limit hit. Back at 4pm." {
			t.Fatalf("server wording %+v", r)
		}
		blank := usageWindowRefusal(ctx, a, refusalWindow(map[string]any{"display_message": "   "}), nil)
		if !strings.HasPrefix(blank.Message, "You've hit your daily limit") {
			t.Fatalf("blank display %q", blank.Message)
		}
	})
	t.Run("test_the_server_sentence_keeps_it_a_window_refusal_when_local_state_is_quiet", func(t *testing.T) {
		setup(t, nil, nil)
		quiet := refusalAgent(t, refusalState{day: 1_200, week: 900})
		refused := refusalWindow(nil)
		refused.Message = "Your included Hermes usage for this week is used up."
		if r := usageWindowRefusal(ctx, quiet, refused, nil); r.Message != "Your included Hermes usage for this week is used up." {
			t.Fatalf("server sentence %q", r.Message)
		}
		if r := usageWindowRefusal(ctx, quiet, refusalWindow(nil), nil); r.Message != "" {
			t.Fatalf("quiet state without a sentence %q", r.Message)
		}
	})
	t.Run("test_the_refusal_names_the_plans_recommended_model", func(t *testing.T) {
		setup(t, map[string]any{
			"example/model-b":         map[string]any{"prompt": "0.0000004", "completion": "0.000004", "name": "Model B"},
			"example/model-free:free": map[string]any{"prompt": "0", "completion": "0", "name": "Model Free"},
		}, []string{"example/model-b", "example/model-free:free"})
		r := usageWindowRefusal(ctx, refusalAgent(t, full), refusalWindow(nil), nil)
		want := []any{map[string]string{"model": "example/model-free:free", "label": "Model Free", "note": "free", "provider": "nous"}}
		if !reflect.DeepEqual(r.Limit["suggestions"], want) {
			t.Fatalf("suggestions %v", r.Limit["suggestions"])
		}
		if !reflect.DeepEqual(r.Limit["credits"], map[string]string{"action": "add_credits"}) {
			t.Fatalf("credits %v", r.Limit["credits"])
		}
	})
	t.Run("test_the_text_never_carries_a_command_hint", func(t *testing.T) {
		setup(t, nil, nil)
		if r := usageWindowRefusal(ctx, refusalAgent(t, full), refusalWindow(nil), nil); r.Message == "" || strings.Contains(r.Message, "/model") {
			t.Fatalf("text %q", r.Message)
		}
	})
	t.Run("test_result_and_error_surface_carry_the_sentence", TestResultAndErrorSurfaceCarryTheUsageWindowSentence)
}
