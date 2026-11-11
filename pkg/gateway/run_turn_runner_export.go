package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs body under the session's frozen launch policy, the policy scope of a turn
func PolicyScope(ctx context.Context, policy *LocalSessionPolicy, a *SessionAuthority, body func(ctx context.Context) error) error {
	return policyScope(ctx, policy, a, body)
}

// Toolsets of a local turn, the policy's current set plus onboarding for an onboarding chat
func PolicyTurnToolsets(ctx context.Context, authority *SessionAuthority, policy *LocalSessionPolicy, cfg map[string]any, sessionID string) ([]string, error) {
	current, err := policy.currentToolsets(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var db *state.DB
	if authority != nil {
		db = authority.DB
	}
	return tools.OnboardingChatToolsets(ctx, slices.Clone(current), db, sessionID), nil
}

// Records an owner side terminal failure on the executing admission and returns it
func RecordTerminalFailure(ctx context.Context, result *pyjson.Object) *pyjson.Object {
	return recordTerminalFailure(ctx, result)
}

// Prunes every tracked turn worker whose thread exited
func PrunePhysicalWorkers(a *SessionAuthority) {
	if a != nil {
		physicalWorkers(a)
	}
}

// Records that the turn streamed its audio so the whole file reply is suppressed
func MarkStreamingTTSCompletedTurn(adapter PlatformAdapter, sessionKey string, turnMarker any) {
	adapter.Base().markStreamingTTSCompletedTurn(sessionKey, turnMarker, nil)
}
