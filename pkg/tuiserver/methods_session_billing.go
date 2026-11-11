package tuiserver

import (
	"context"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var sessionBillingRegistry = &handlerRegistry{}

func init() {
	sessionBillingRegistry.scopedMethod("billing.state", billingState)
	billingView("usage.bars", func(ctx context.Context) map[string]any {
		return serializeUsageModel(provider.BuildUsageModel(ctx, 10*time.Second))
	}, map[string]any{"ok": true, "available": false})
	billingView("subscription.state", func(ctx context.Context) map[string]any {
		state, err := provider.BuildSubscriptionState(ctx, 15*time.Second)
		panicIf(err)
		return serializeSubscriptionState(ctx, state)
	}, map[string]any{"ok": true, "logged_in": false, "error": "could not load subscription state"})
	sessionBillingRegistry.scopedMethod("subscription.preview", subscriptionPreview)
	billingRoute("subscription.change", func(env provider.PoolEnv, p map[string]any, _ any) map[string]any {
		result, err := provider.PutNousSubscriptionPendingChange(env, p["subscription_type_id"], truthy(p["cancel"]), provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPendingChange(result)
	}, func(p map[string]any) bool { return !truthy(p["cancel"]) && !truthy(p["subscription_type_id"]) },
		"subscription_type_id or cancel is required", "invalid_request", false)
	billingRoute("subscription.resume", func(env provider.PoolEnv, p map[string]any, _ any) map[string]any {
		result, err := provider.DeleteNousSubscriptionPendingChange(env, provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPendingChange(result)
	}, nil, "", "invalid_request", false)
	billingRoute("subscription.upgrade", func(env provider.PoolEnv, p map[string]any, key any) map[string]any {
		result, err := provider.PostNousSubscriptionUpgrade(env, p["subscription_type_id"], key, provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPick(result, [2]string{"status", "status"}, [2]string{"target_tier_name", "targetTierName"},
			[2]string{"recovery_url", "recoveryUrl"}, [2]string{"reason", "reason"})
	}, func(p map[string]any) bool { return !truthy(p["subscription_type_id"]) }, "subscription_type_id is required", "invalid_request", true)
	billingRoute("billing.charge", func(env provider.PoolEnv, p map[string]any, key any) map[string]any {
		result, err := provider.PostNousBillingCharge(env, p["amount_usd"], key, provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPick(result, [2]string{"charge_id", "chargeId"})
	}, func(p map[string]any) bool { return p["amount_usd"] == nil }, "amount_usd is required", "invalid_request", true)
	billingRoute("billing.charge_status", func(env provider.PoolEnv, p map[string]any, _ any) map[string]any {
		result, err := provider.GetNousChargeStatus(env, p["charge_id"], provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPick(result, [2]string{"status", "status"}, [2]string{"amount_usd", "amountUsd"},
			[2]string{"settled_at", "settledAt"}, [2]string{"reason", "reason"})
	}, func(p map[string]any) bool { return !truthy(p["charge_id"]) }, "charge_id is required", "invalid_charge_id", false)
	billingRoute("billing.auto_reload", func(env provider.PoolEnv, p map[string]any, _ any) map[string]any {
		_, err := provider.PatchNousAutoTopUp(env, truthy(p["enabled"]), p["threshold"], p["top_up_amount"], provider.NousBillingDefaultTimeout)
		panicIf(err)
		return map[string]any{"ok": true}
	}, func(p map[string]any) bool { return p["threshold"] == nil || p["top_up_amount"] == nil },
		"threshold and top_up_amount are required", "invalid_request", false)
	billingRoute("billing.keep_going_on_credits", func(env provider.PoolEnv, p map[string]any, _ any) map[string]any {
		result, err := provider.PatchNousKeepGoingOnCredits(env, p["enabled"].(bool), provider.NousBillingDefaultTimeout)
		panicIf(err)
		return billingPick(result)
	}, func(p map[string]any) bool { _, isBool := p["enabled"].(bool); return !isBool }, "enabled (bool) is required", "invalid_request", false)
	sessionBillingRegistry.scopedMethod("billing.step_up", billingStepUp)
	sessionBillingRegistry.install()
}

// Profile bound read only view answering its fallback on any failure
func billingView(name string, build func(ctx context.Context) map[string]any, fallback map[string]any) {
	sessionBillingRegistry.scopedMethod(name, func(ctx context.Context, rid any, params map[string]any) map[string]any {
		var result map[string]any
		if failure := catchPanic(func() { result = build(ctx) }); failure != nil {
			return ok(rid, copyDict(fallback))
		}
		return ok(rid, result)
	})
}

// Portal write route refusing invalid params and echoing a minted idempotency key on success and error
func billingRoute(name string, call func(env provider.PoolEnv, params map[string]any, key any) map[string]any, invalid func(map[string]any) bool, message, errorKind string, idempotent bool) {
	sessionBillingRegistry.scopedMethod(name, func(ctx context.Context, rid any, params map[string]any) map[string]any {
		if invalid != nil && invalid(params) {
			return billingInvalid(rid, message, errorKind)
		}
		var key any
		var extra map[string]any
		if idempotent {
			key = params["idempotency_key"]
			if !truthy(key) {
				key = provider.NewIdempotencyKey()
			}
			extra = map[string]any{"idempotency_key": key}
		}
		return billingCall(rid, func() map[string]any {
			result := call(scopedPoolEnv(ctx), params, key)
			for k, v := range extra {
				result[k] = v
			}
			return result
		}, extra)
	})
}

// Profile bound billing view, the free tier answered locally without a portal round trip
func billingState(ctx context.Context, rid any, params map[string]any) map[string]any {
	var result map[string]any
	if failure := catchPanic(func() {
		if provider.HasFreeTierAccount(scopedPoolEnv(ctx)) {
			result = serializeBillingState(ctx, &provider.BillingState{LoggedIn: false}, true)
			return
		}
		state, err := provider.BuildBillingState(ctx, 15*time.Second)
		panicIf(err)
		result = serializeBillingState(ctx, state, false)
	}); failure != nil {
		return ok(rid, map[string]any{"ok": true, "logged_in": false, "free_tier_account": false, "error": "could not load billing state"})
	}
	return ok(rid, result)
}

// Chargeless effect quote of a subscription change
func subscriptionPreview(ctx context.Context, rid any, params map[string]any) map[string]any {
	tierID := params["subscription_type_id"]
	if !truthy(tierID) {
		return billingInvalid(rid, "subscription_type_id is required", "invalid_request")
	}
	return billingCall(rid, func() map[string]any {
		raw, err := provider.PostNousSubscriptionPreview(scopedPoolEnv(ctx), tierID, provider.NousBillingDefaultTimeout)
		panicIf(err)
		payload, _ := raw.(map[string]any)
		preview, err := provider.SubscriptionChangePreviewFromPayload(payload)
		panicIf(err)
		return serializeSubscriptionPreview(preview)
	}, nil)
}

// billing manage step up device flow whose verification url and code reach the TUI as an event
func billingStepUp(ctx context.Context, rid any, params map[string]any) map[string]any {
	sid := strOr(params["session_id"])
	return billingCall(rid, func() map[string]any {
		granted, err := provider.StepUpNousBillingScope(scopedPoolEnv(ctx), io.Discard, false, 15*time.Second, func(url, code string) {
			tuigateway.Emit(ctx, "billing.step_up.verification", sid, map[string]any{"verification_url": url, "user_code": code})
		})
		panicIf(err)
		return map[string]any{"ok": true, "granted": granted}
	}, map[string]any{"granted": false})
}
