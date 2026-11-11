package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Delivers a watch event to a stateless api_server session by raw id, a delegation as a durable row
func (r *GatewayRunner) selfPostAPIServer(ctx context.Context, adapter gateway.PlatformAdapter, synthText, rawSID string, evt map[string]any) (bool, error) {
	var info, fail string
	var deliver func(ctx context.Context) error
	scope := ctx
	if evt["type"] == "async_delegation" {
		info = "Async delegation completion — persisting delivery row for api_server session %s (no wake turn)"
		fail = "Async delegation delivery persist failed for session %s: %s"
		deliver = func(ctx context.Context) error {
			return gateway.PersistDelegationDelivery(ctx, adapter, synthText, rawSID, evt)
		}
	} else {
		info = "Watch pattern notification — waking api_server session %s via self-post"
		fail = "Watch notification self-post wake failed for session %s: %s"
		served, err := r.servedAPIServerWakeProfile(ctx, evt, rawSID)
		var lookup *lookupError
		if errors.As(err, &lookup) {
			inboundLog().Warn(fmt.Sprintf(fail, rawSID, err))
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if served != "" {
			profile := served
			source := &gateway.SessionSource{Platform: gateway.PlatformAPIServer, ChatID: rawSID, Profile: &profile}
			profileHome, err := r.resolveProfileHomeForSource(ctx, source)
			if err != nil {
				return false, err
			}
			scope = gateway.ProfileRuntimeScope(ctx, profileHome, true)
		}
		category := "result"
		if agent.DiagnosticProcessEvent(evt) {
			category = "diagnostic"
		}
		deliver = func(ctx context.Context) error {
			return gateway.DeliverWake(ctx, adapter, gateway.WakeOptions{Text: gateway.MarkInternalNotification(synthText),
				SessionID: rawSID, Profile: served, NotificationCategory: category})
		}
	}
	inboundLog().Info(fmt.Sprintf(info, rawSID))
	if err := deliver(scope); err != nil {
		inboundLog().Warn(fmt.Sprintf(fail, rawSID, err))
		return false, nil
	}
	return true, nil
}

// Producer stable identity of a completion event, false when the event carries none
func completionDeliveryIdentity(evt map[string]any) (completionIdentity, bool) {
	evtType := truthyText(evt["type"])
	switch evtType {
	case "async_delegation":
		producer := truthyText(evt["delegation_id"])
		if producer == "" {
			return completionIdentity{}, false
		}
		if pyrepr.Truthy(evt["task_failure_notice"]) {
			index := ""
			results, _ := evt["results"].([]any)
			if len(results) > 0 {
				if first, ok := results[0].(map[string]any); ok {
					if v, ok := first["task_index"]; ok {
						index = pyrepr.Str(v)
					}
				}
			}
			return completionIdentity{evtType: evtType, producerID: producer, incarnation: "task_failure:" + index}, true
		}
		return completionIdentity{evtType: evtType, producerID: producer, incarnation: ""}, true
	case "watch_match", "watch_disabled", "heartbeat":
		if pyrepr.Truthy(evt["event_id"]) {
			return completionIdentity{evtType: evtType, producerID: pyrepr.Str(evt["session_id"]), incarnation: pyrepr.Str(evt["event_id"])}, true
		}
	case "completion":
		producer := truthyText(evt["session_id"])
		if startedAt, ok := evt["started_at"]; ok && producer != "" && startedAt != nil {
			return completionIdentity{evtType: evtType, producerID: producer, incarnation: startedAt}, true
		}
	}
	return completionIdentity{}, false
}

// Moves identities from inflight to delivered and trims the oldest past retention, lock held by the caller
func (r *GatewayRunner) markCompletionsDeliveredLocked(identities []completionIdentity) {
	for _, identity := range identities {
		delete(r.completionDeliveriesInflight, identity)
		r.completionDeliveriesDelivered.Set(identity, struct{}{})
	}
	for r.completionDeliveriesDelivered.Len() > r.completionDeliveryRetention {
		r.completionDeliveriesDelivered.Delete(r.completionDeliveriesDelivered.Keys()[0])
	}
}

// Whether the identity is inflight or delivered this lifecycle, claiming an unseen one when asked
func (r *GatewayRunner) completionIdentitySeen(identity completionIdentity, claim bool) bool {
	r.completionDeliveryLock.Lock()
	defer r.completionDeliveryLock.Unlock()
	_, inflight := r.completionDeliveriesInflight[identity]
	seen := inflight || r.completionDeliveriesDelivered.Has(identity)
	if claim && !seen {
		r.completionDeliveriesInflight[identity] = struct{}{}
	}
	return seen
}

// Deliver, terminal or retry verdict of an async completion target before adapter acceptance
func (r *GatewayRunner) classifyCompletionTarget(ctx context.Context, parentSessionID, sessionKey string) (string, error) {
	verdict, _, _, err := r.classifyCompletionOwner(ctx, parentSessionID, sessionKey)
	return verdict, err
}

// Drops, releases, defers or completes a durable completion claim, failures logged
func settleDurableClaim(ctx context.Context, kind, delegationID, claimID string) {
	op := durableClaimOps[kind]
	if _, err := op.fn(inboundToolsEnv(ctx), delegationID, claimID); err != nil {
		level := slog.LevelDebug
		if kind == "complete" {
			level = slog.LevelWarn
		}
		inboundLog().Log(ctx, level, op.failMsg, "error", err)
	}
}
