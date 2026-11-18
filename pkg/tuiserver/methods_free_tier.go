package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
)

var freeTierRegistry = &handlerRegistry{}

func init() {
	freeTierRegistry.scopedMethod("free_tier.status", freeTierStatus)
	freeTierRegistry.scopedMethod("free_tier.ack_notice", freeTierAckNotice)
	freeTierRegistry.scopedMethod("free_tier.claim_nudge", freeTierClaimNudge)
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

// Guest, trial, notice and sign in offer state of the focused profile, never creating an identity
func freeTierStatus(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer freeTierFailure(rid, 5090, &response)
	env := scopedPoolEnv(ctx)
	hasGuest := provider.HasGuest(env)
	available := provider.FreeInferenceActive(env)
	payload := map[string]any{"has_guest": hasGuest, "enabled": available, "available": available,
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
	if available && !hasGuest {
		for key, value := range provider.LastMintFailurePayload(env) {
			payload[key] = value
		}
	}
	if challenge := provider.PendingChallenge(env); challenge != nil {
		payload["challenge"] = challenge
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
