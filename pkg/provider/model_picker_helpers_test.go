package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Far reset stamp the picker fixtures use, thirty days ahead
var pickerFar = time.Now().UnixMilli() + 30*86_400_000

// English plan usage catalog entries standing in for the i18n slice
var pickerEnglish = map[string]string{
	"plan_usage.level_free":               "Free",
	"plan_usage.level_top_tier":           "Large",
	"plan_usage.resets_in":                "resets in {duration}",
	"plan_usage.resets_soon":              "resets soon",
	"plan_usage.limit_reached":            "{name} limit reached · {resets}",
	"plan_usage.limit_day":                "daily",
	"plan_usage.limit_week":               "weekly",
	"plan_usage.limit_month":              "monthly",
	"plan_usage.limit_top_tier":           "monthly large-model",
	"plan_usage.then_credits":             "paid models now use your credits (pay as you go)",
	"plan_usage.then_credits_large":       "Large models now use your credits (pay as you go)",
	"plan_usage.then_wait":                "only Free models until then",
	"plan_usage.then_wait_large":          "Large models paused until then",
	"plan_usage.large_notice":             "Large model: uses more of your plan's usage per message.",
	"plan_usage.large_notice_credits":     "Large model: uses more credits per message (pay as you go).",
	"plan_usage.large_notice_limit":       "Large model: your {name} limit is reached ({resets}).",
	"plan_usage.large_notice_limit_stops": "Large model: your {name} limit is reached ({resets}), so it won't run until then.",
	"plan_usage.large_notice_percent":     "Large model: uses more of your plan's usage per message. Monthly large-model limit: {percent} used.",
}

// Answers plan usage catalog lookups from the English entries for the test
func installPickerEnglish(t *testing.T) {
	t.Helper()
	prev := pickerTranslateFn
	pickerTranslateFn = func(key, lang string, kwargs map[string]any) string {
		text, ok := pickerEnglish[key]
		if !ok {
			t.Fatalf("missing catalog key %s", key)
		}
		for k, v := range kwargs {
			text = strings.ReplaceAll(text, "{"+k+"}", fmt.Sprint(v))
		}
		return text
	}
	t.Cleanup(func() { pickerTranslateFn = prev })
}

func pickerI64(v int64) *int64 { return &v }

// Python style keyword arguments of the usage window fixture
type pickerKW map[string]any

func (kw pickerKW) get(key string, def any) any {
	if v, ok := kw[key]; ok {
		return v
	}
	return def
}

func pickerResetArg(v any) *int64 {
	switch x := v.(type) {
	case int64:
		return &x
	case int:
		r := int64(x)
		return &r
	}
	return nil
}

// Server gauges for day, week, optional month and optional top tier allowances in server order
func pickerWindowGauges(kw pickerKW) []WindowGauge {
	dayReset := pickerResetArg(kw.get("day_resets_at_ms", pickerFar))
	out := []WindowGauge{
		{ID: "day", Scope: "all", Label: "Today", UsedBp: kw.get("day_used_bp", 0).(int), ResetsAtMs: dayReset},
		{ID: "week", Scope: "all", Label: "This week", UsedBp: kw.get("week_used_bp", 0).(int), ResetsAtMs: pickerResetArg(kw.get("week_resets_at_ms", pickerFar))},
	}
	if month, ok := kw.get("month_used_bp", nil).(int); ok {
		out = append(out, WindowGauge{ID: "month", Scope: "all", Label: "This month", UsedBp: month, ResetsAtMs: pickerResetArg(kw.get("month_resets_at_ms", pickerFar))})
	}
	if top, ok := kw.get("top_tier_used_bp", 0).(int); ok {
		reset := pickerResetArg(kw.get("top_tier_resets_at_ms", nil))
		if reset == nil {
			reset = dayReset
		}
		out = append(out, WindowGauge{ID: "top_tier", Scope: "top_tier", Label: "Large models (monthly)", UsedBp: top, ResetsAtMs: reset})
	}
	return out
}

// Logged in account snapshot on a usage window plan built from the fixture keywords
func pickerWindowAccount(plan string, kw pickerKW) *NousPortalAccountInfo {
	window := &NousUsageWindowInfo{Gauges: pickerWindowGauges(kw)}
	if f, ok := kw["featured_model_ids"].([]string); ok {
		window.FeaturedModelIDs = f
	}
	if r, ok := kw["paused_reason"].(string); ok {
		window.PausedReason = r
	}
	if k, ok := kw["keep_going_on_credits"].(bool); ok {
		window.KeepGoingOnCredits = &k
	}
	info := &NousPortalAccountInfo{LoggedIn: true, UsageWindow: window, Subscription: &NousPortalSubscriptionInfo{Plan: &plan}}
	if c, ok := kw["purchased_credits_remaining"].(float64); ok {
		info.PaidServiceAccessInfo = &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: &c}
	}
	return info
}

// Window plan usage where the gateway marked Model B top tier
func pickerWindowUsage(kw pickerKW) *PickerUsage {
	u := pickerUsageFromAccount(pickerWindowAccount("Window Plan", kw))
	if u != nil {
		u.TopTierIDs = map[string]bool{"example/model-b": true}
	}
	return u
}

// Copy of the usage with the credits pay answer replaced
func pickerWithCredits(u *PickerUsage, pays *bool) *PickerUsage {
	cp := *u
	cp.CreditsPay = pays
	return &cp
}

func pickerBool(b bool) *bool { return &b }

// Scripts the cached and fetched account snapshots, returning the force fresh flag of every fetch
func scriptPickerAccounts(t *testing.T, stale, fetched *NousPortalAccountInfo) *[]bool {
	t.Helper()
	calls := &[]bool{}
	var fresh *NousPortalAccountInfo
	prevCached, prevFetch := pickerCachedAccountFn, pickerFetchAccountFn
	pickerCachedAccountFn = func(ctx context.Context, anyAge bool) *NousPortalAccountInfo {
		if fresh != nil {
			return fresh
		}
		if anyAge {
			return stale
		}
		return nil
	}
	pickerFetchAccountFn = func(ctx context.Context, forceFresh bool) *NousPortalAccountInfo {
		*calls = append(*calls, forceFresh)
		if forceFresh {
			fresh = fetched
		}
		return fetched
	}
	t.Cleanup(func() { pickerCachedAccountFn, pickerFetchAccountFn = prevCached, prevFetch })
	return calls
}

// Serves one account snapshot as the fresh and the any age cache
func servePickerSnapshot(t *testing.T, info *NousPortalAccountInfo) {
	t.Helper()
	prevCached := pickerCachedAccountFn
	pickerCachedAccountFn = func(context.Context, bool) *NousPortalAccountInfo { return info }
	t.Cleanup(func() { pickerCachedAccountFn = prevCached })
}

// Serves the given Nous catalog rows and records the cached only flag of every read
func servePickerCatalog(t *testing.T, rows PricingTable) *[]bool {
	t.Helper()
	seen := &[]bool{}
	prev := pickerPricingFn
	pickerPricingFn = func(ctx context.Context, provider, baseURL string, forceRefresh, cachedOnly bool) PricingTable {
		*seen = append(*seen, cachedOnly)
		return rows
	}
	t.Cleanup(func() { pickerPricingFn = prev })
	return seen
}
