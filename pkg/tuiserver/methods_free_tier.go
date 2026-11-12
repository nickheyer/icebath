package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/provider"
)

var freeTierRegistry = &handlerRegistry{}

func init() {
	freeTierRegistry.scopedMethod("free_tier.status", freeTierStatus)
	freeTierRegistry.scopedMethod("free_tier.provision", freeTierProvision)
	freeTierRegistry.scopedMethod("free_tier.ack_notice", freeTierAckNotice)
	freeTierRegistry.scopedMethod("free_tier.claim_nudge", freeTierClaimNudge)
	freeTierRegistry.scopedMethod("free_tier.challenge_result", freeTierChallengeResult)
	freeTierRegistry.install()
}

// Answers a raised failure of a free tier handler with its code and message, a config outage passing through
func freeTierFailure(rid any, code int, response *map[string]any) {
	failure := recover()
	if failure == nil {
		return
	}
	if configOutage(failure) {
		panic(failure)
	}
	*response = rpcErr(rid, code, failureText(failure), nil)
}

// Whether nothing blocks a welcome trial claim here
func trialClaimable(ctx context.Context) bool {
	blocker, err := provider.WelcomeClaimBlocker(scopedPoolEnv(ctx))
	if err != nil {
		panic(err)
	}
	return blocker == ""
}

// Guest, trial, notice and sign in offer state of the focused profile, never creating an identity
func freeTierStatus(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer freeTierFailure(rid, 5090, &response)
	env := scopedPoolEnv(ctx)
	hasGuest := provider.HasGuest(env)
	available := provider.FreeInferenceActive(env)
	enabled := available || trialClaimable(ctx)
	payload := map[string]any{"has_guest": hasGuest, "enabled": enabled, "available": available,
		"notice_pending": available && provider.GuestNoticePending(env), "model": provider.GuestModel, "label": provider.FreeTierLabel}
	for _, offer := range [][2]string{{"nudge_due_in", provider.FreeTierOfferTrial}, {"connector_nudge_due_in", provider.FreeTierOfferConnector}} {
		dueIn, pending, err := provider.OfferDueIn(env, offer[1])
		if err != nil {
			panic(err)
		}
		if pending {
			payload[offer[0]] = dueIn
		}
	}
	if enabled && !hasGuest {
		for key, value := range provider.LastMintFailurePayload(env) {
			payload[key] = value
		}
	}
	if challenge := provider.PendingChallenge(env); len(challenge) > 0 {
		payload["challenge"] = challenge
	}
	return ok(rid, payload)
}

// Retries a failed welcome trial claim once inside the cooldown when nothing blocks it
func freeTierProvision(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer freeTierFailure(rid, 5092, &response)
	env := scopedPoolEnv(ctx)
	if trialClaimable(ctx) {
		if _, err := provider.ClaimDesktopFreeTier(env, false, true); err != nil {
			var authErr *provider.AuthError
			if !errors.As(err, &authErr) {
				panic(err)
			}
			logger().Info("welcome trial claim failed: " + err.Error())
		}
		if !truthy(params["profile"]) {
			provider.RefreshSetupRecord(env, true)
		}
	}
	hasGuest := provider.HasGuest(env)
	enabled := provider.FreeInferenceActive(env) || trialClaimable(ctx)
	payload := map[string]any{"has_guest": hasGuest, "enabled": enabled}
	if enabled && !hasGuest {
		for key, value := range provider.LastMintFailurePayload(env) {
			payload[key] = value
		}
	}
	return ok(rid, payload)
}

// Marks the availability notice shown on the free tier identity
func freeTierAckNotice(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer freeTierFailure(rid, 5091, &response)
	acked, err := provider.MarkGuestNoticeShown(scopedPoolEnv(ctx))
	if err != nil {
		panic(err)
	}
	return ok(rid, map[string]any{"acked": acked})
}

// Claims the due sign in offer of a kind for exactly one caller each time it comes due
func freeTierClaimNudge(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			logger().Warn("free_tier.claim_nudge failed", "error", failure)
			response = rpcErr(rid, 5093, failureText(failure), nil)
		}
	}()
	kind := provider.FreeTierOfferTrial
	if truthy(params["kind"]) {
		kind = relayString(params["kind"])
	}
	claimed, err := provider.ClaimOffer(scopedPoolEnv(ctx), kind)
	if err != nil {
		panic(err)
	}
	return ok(rid, map[string]any{"claimed": claimed})
}

// Records the browser challenge outcome the host saw for the pending attempt
func freeTierChallengeResult(ctx context.Context, rid any, params map[string]any) map[string]any {
	rawURL, present := params["url"]
	if !present {
		panic(errors.New("'url'"))
	}
	outcome, present := params["outcome"]
	if !present {
		panic(errors.New("'outcome'"))
	}
	attempt, numeric := 0, true
	if raw, given := params["attempt"]; given {
		switch value := raw.(type) {
		case int:
			attempt = value
		case bool:
			attempt = map[bool]int{false: 0, true: 1}[value]
		case float64:
			attempt, numeric = int(value), value == float64(int(value))
		default:
			numeric = false
		}
	}
	text, isText := rawURL.(string)
	if !isText || !numeric {
		return ok(rid, map[string]any{"accepted": false})
	}
	return ok(rid, map[string]any{"accepted": provider.RecordHostOutcome(scopedPoolEnv(ctx), text, attempt, relayString(outcome))})
}
