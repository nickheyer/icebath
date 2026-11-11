package tuiserver

import (
	"context"
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Decimal as its wire string, nil passing through
func wireStr(value *provider.PyDecimal) any {
	if value == nil {
		return nil
	}
	return value.String()
}

// Optional string pointer as its value or nil
func optStr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// Money display of a decimal, the format_money of the billing view
func formatMoney(value *provider.PyDecimal) string {
	text, err := provider.FormatMoney(value, false)
	panicIf(err)
	return text
}

// Snake keyed picks of camel keyed portal fields in order behind ok true
func billingPick(result any, fields ...[2]string) map[string]any {
	source, _ := result.(map[string]any)
	out := map[string]any{"ok": true}
	for _, field := range fields {
		out[field[0]] = source[field[1]]
	}
	return out
}

// Result error envelope of a billing failure the TUI branches on
func serializeBillingError(exc *provider.BillingError) map[string]any {
	kind := map[provider.BillingErrorKind]string{provider.BillingErrorRemoteSpendingRevoked: "remote_spending_revoked",
		provider.BillingErrorSessionRevoked: "session_revoked", provider.BillingErrorScopeRequired: "insufficient_scope"}[exc.Kind]
	if kind == "" {
		kind = "error"
		if exc.Kind.IsTransient() {
			kind = "rate_limited"
		}
		if truthy(exc.ErrorCode) {
			kind = strOr(exc.ErrorCode)
		}
	}
	var retryAfter any
	if exc.RetryAfter != nil {
		retryAfter = *exc.RetryAfter
	}
	payload := map[string]any{}
	if len(exc.Payload) > 0 {
		payload = exc.Payload
	}
	return map[string]any{"ok": false, "error": kind, "message": exc.Error(), "portal_url": exc.PortalURL, "retry_after": retryAfter,
		"payload": payload, "actor": exc.Actor, "code": exc.Code, "recovery": exc.Recovery}
}

// Payment method fields of its own kind only
func serializePaymentMethod(pm *provider.PaymentMethodInfo) map[string]any {
	if pm == nil {
		return nil
	}
	switch pm.Kind {
	case "card":
		return map[string]any{"kind": "card", "brand": optStr(pm.Brand), "last4": optStr(pm.Last4), "wallet": optStr(pm.Wallet),
			"resolved_via": optStr(pm.ResolvedVia)}
	case "link":
		return map[string]any{"kind": "link", "email": optStr(pm.Email), "resolved_via": optStr(pm.ResolvedVia)}
	}
	return map[string]any{"kind": "unknown", "raw_kind": optStr(pm.RawKind), "resolved_via": optStr(pm.ResolvedVia)}
}

// Auto reload settings with money displays and the card it charges
func serializeAutoReload(ar *provider.AutoReload) map[string]any {
	if ar == nil {
		return nil
	}
	var cardOut map[string]any
	if ar.Card != nil {
		cardOut = map[string]any{"kind": ar.Card.Kind}
		if ar.Card.Kind == "distinct" {
			cardOut["payment_method_id"], cardOut["brand"], cardOut["last4"] = optStr(ar.Card.PaymentMethodID), optStr(ar.Card.Brand), optStr(ar.Card.Last4)
		}
	}
	return map[string]any{"enabled": ar.Enabled, "threshold_usd": wireStr(ar.ThresholdUSD), "threshold_display": formatMoney(ar.ThresholdUSD),
		"reload_to_usd": wireStr(ar.ReloadToUSD), "reload_to_display": formatMoney(ar.ReloadToUSD), "card": cardOut}
}

// Must of a capability read of the billing state
func mustBool(value bool, err error) bool {
	panicIf(err)
	return value
}

// Wire billing state with money safe strings, the free tier marked before logged_in
func serializeBillingState(ctx context.Context, state *provider.BillingState, freeTierAccount bool) map[string]any {
	var card, mc map[string]any
	if state.Card != nil {
		card = map[string]any{"brand": state.Card.Brand, "last4": state.Card.Last4, "masked": state.Card.Masked(),
			"display": state.Card.Display(), "resolved_via": optStr(state.Card.ResolvedVia)}
	}
	if m := state.MonthlyCap; m != nil {
		mc = map[string]any{"limit_usd": wireStr(m.LimitUSD), "limit_display": formatMoney(m.LimitUSD),
			"spent_this_month_usd": wireStr(m.SpentThisMonthUSD), "spent_display": formatMoney(m.SpentThisMonthUSD),
			"is_default_ceiling": m.IsDefaultCeiling}
	}
	var freeTierModel, keepGoing, stateError any
	if freeTierAccount {
		freeTierModel = provider.GuestModel
	}
	if state.KeepGoingOnCredits != nil {
		keepGoing = *state.KeepGoingOnCredits
	}
	if state.Error != nil {
		stateError = *state.Error
	}
	presets, presetDisplays := []any{}, []any{}
	for i := range state.ChargePresets {
		presets = append(presets, wireStr(&state.ChargePresets[i]))
		presetDisplays = append(presetDisplays, formatMoney(&state.ChargePresets[i]))
	}
	return map[string]any{"ok": true, "logged_in": state.LoggedIn, "free_tier_account": freeTierAccount, "free_tier_model": freeTierModel,
		"org_name": state.OrgName, "org_slug": state.OrgSlug, "role": state.Role, "is_admin": mustBool(state.IsAdmin()),
		"can_change_plan": mustBool(state.CanChangePlan()), "can_charge": mustBool(state.CanCharge()),
		"balance_usd": wireStr(state.BalanceUSD), "balance_display": formatMoney(state.BalanceUSD),
		"cli_billing_enabled": state.CLIBillingEnabled, "charge_presets": presets, "charge_presets_display": presetDisplays,
		"min_usd": wireStr(state.MinUSD), "max_usd": wireStr(state.MaxUSD), "card": card,
		"payment_method": serializePaymentMethod(state.PaymentMethod), "monthly_cap": mc, "auto_reload": serializeAutoReload(state.AutoReload),
		"keep_going_on_credits": keepGoing, "portal_url": state.PortalURL, "error": stateError,
		"usage": usagePayload(ctx, state.LoggedIn), "plan_usage": planUsagePayload(ctx, state.LoggedIn)}
}

// Usage window gauges of a logged in window plan from the cached snapshot, nil for every other account
func planUsagePayload(ctx context.Context, loggedIn bool) (payload map[string]any) {
	if !loggedIn {
		return nil
	}
	if failure := catchPanic(func() {
		snapshot := provider.CachedNousPortalAccountInfo(ctx, true)
		if snapshot == nil || snapshot.UsageWindow == nil || len(provider.AllScope(snapshot.UsageWindow.Gauges)) == 0 {
			return
		}
		if usage := provider.NousPickerUsage(true, false); usage != nil {
			payload = usage.ToPayload()
		}
	}); failure != nil {
		logger().Debug("Plan usage payload unavailable", "error", failure)
		return nil
	}
	return payload
}

// Shared two bar usage model of a logged in account, unavailable on any failure
func usagePayload(ctx context.Context, loggedIn bool) (payload map[string]any) {
	if !loggedIn {
		return map[string]any{"available": false}
	}
	if failure := catchPanic(func() { payload = serializeUsageModel(provider.BuildUsageModel(ctx, 10*time.Second)) }); failure != nil {
		return map[string]any{"available": false}
	}
	return payload
}

// Usage bar with dollar displays and fractions
func serializeUsageBar(bar *provider.UsageBar) map[string]any {
	if bar == nil {
		return nil
	}
	var pct any
	if n, ok := bar.PctUsed(); ok {
		pct = n
	}
	remaining, total, spent := bar.RemainingUSD, bar.TotalUSD, bar.SpentUSD
	return map[string]any{"kind": bar.Kind, "remaining_display": provider.FormatUSD(&remaining), "total_display": provider.FormatUSD(&total),
		"spent_display": provider.FormatUSD(&spent), "pct_used": pct, "fill_fraction": bar.FillFraction()}
}

// Optional string as nil when empty
func noneIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Renewal display of a date, nil when it does not parse
func formatRenews(value any) any {
	text, isStr := value.(string)
	if !isStr {
		return nil
	}
	if display, ok := provider.FormatRenews(text); ok {
		return display
	}
	return nil
}

// Wire usage model of the shared two bar dollar view
func serializeUsageModel(model provider.UsageModel) map[string]any {
	if !model.Available {
		return map[string]any{"ok": true, "available": false}
	}
	usd := func(value *float64) any {
		if value == nil {
			return nil
		}
		return provider.FormatUSD(value)
	}
	renews := noneIfEmpty(model.RenewsDisplay)
	if renews == nil {
		renews = formatRenews(noneIfEmpty(model.RenewsAt))
	}
	return map[string]any{"ok": true, "available": true, "status": model.Status, "plan_name": noneIfEmpty(model.PlanName),
		"renews_at": noneIfEmpty(model.RenewsAt), "renews_display": renews,
		"subscription_remaining_display": usd(model.SubscriptionRemainingUSD), "topup_remaining_display": usd(model.TopupRemainingUSD),
		"total_spendable_display": usd(model.TotalSpendableUSD), "has_topup": model.HasTopup(),
		"plan_bar": serializeUsageBar(model.PlanBar), "topup_bar": serializeUsageBar(model.TopupBar)}
}

// Wire subscription state with its tier catalog and usage
func serializeSubscriptionState(ctx context.Context, state *provider.SubscriptionState) map[string]any {
	var current map[string]any
	if c := state.Current; c != nil {
		current = map[string]any{"tier_id": c.TierID, "tier_name": c.TierName, "monthly_credits": wireStr(c.MonthlyCredits),
			"credits_remaining": wireStr(c.CreditsRemaining), "cycle_ends_at": c.CycleEndsAt,
			"pending_downgrade_tier_name": c.PendingDowngradeTierName, "pending_downgrade_at": c.PendingDowngradeAt,
			"pending_downgrade_display": formatRenews(c.PendingDowngradeAt), "cancel_at_period_end": c.CancelAtPeriodEnd,
			"cancellation_effective_at": c.CancellationEffectiveAt, "cancellation_effective_display": formatRenews(c.CancellationEffectiveAt)}
	}
	tiers := []any{}
	for _, t := range state.Tiers {
		tiers = append(tiers, map[string]any{"tier_id": t.TierID, "name": t.Name, "tier_order": t.TierOrder,
			"dollars_per_month_display": formatMoney(t.DollarsPerMonth), "monthly_credits": wireStr(t.MonthlyCredits),
			"is_current": t.IsCurrent, "is_enabled": t.IsEnabled})
	}
	var stateError any
	if state.Error != nil {
		stateError = *state.Error
	}
	return map[string]any{"ok": true, "logged_in": state.LoggedIn, "is_admin": mustBool(state.IsAdmin()),
		"can_change_plan": mustBool(state.CanChangePlan()), "org_name": state.OrgName, "org_id": state.OrgID, "role": state.Role,
		"context": noneIfEmpty(state.Context), "current": current, "tiers": tiers, "portal_url": state.PortalURL, "error": stateError,
		"usage": usagePayload(ctx, state.LoggedIn), "plan_usage": planUsagePayload(ctx, state.LoggedIn)}
}

// Wire subscription change preview with the credit delta as a string
func serializeSubscriptionPreview(p *provider.SubscriptionChangePreview) map[string]any {
	var due any
	if p.AmountDueNowCents != nil {
		due = *p.AmountDueNowCents
	}
	return map[string]any{"ok": true, "effect": p.Effect, "reason": p.Reason, "current_tier_id": p.CurrentTierID,
		"current_tier_name": p.CurrentTierName, "target_tier_id": p.TargetTierID, "target_tier_name": p.TargetTierName,
		"monthly_credits_delta": wireStr(p.MonthlyCreditsDelta), "amount_due_now_cents": due, "effective_at": p.EffectiveAt}
}

// Portal call answered ok, a billing failure as its typed envelope, others generic, extra on both
func billingCall(rid any, fn func() map[string]any, extra map[string]any) map[string]any {
	var result map[string]any
	failure := catchPanic(func() { result = fn() })
	if failure == nil {
		return ok(rid, result)
	}
	var envelope map[string]any
	var billingErr *provider.BillingError
	if asErr, isErr := failure.(error); isErr && errors.As(asErr, &billingErr) {
		envelope = serializeBillingError(billingErr)
	} else {
		message := strOr(failure)
		if asErr, isErr := failure.(error); isErr {
			message = asErr.Error()
		}
		envelope = map[string]any{"ok": false, "error": "error", "message": message}
	}
	for key, value := range extra {
		envelope[key] = value
	}
	return ok(rid, envelope)
}

// Structured invalid request envelope
func billingInvalid(rid any, message, errorKind string) map[string]any {
	return ok(rid, map[string]any{"ok": false, "error": errorKind, "message": message})
}

// Pending change result with its message
func billingPendingChange(result any) map[string]any {
	source, _ := result.(map[string]any)
	return map[string]any{"ok": true, "message": source["message"], "payload": result}
}
