package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Runs body under the session's frozen launch policy, the policy scope of a turn
func PolicyScope(ctx context.Context, policy *LocalSessionPolicy, a *SessionAuthority, body func(ctx context.Context) error) error {
	return policyScope(ctx, policy, a, body)
}

// Toolsets of a local turn, a copy of the policy's current set
func PolicyTurnToolsets(ctx context.Context, policy *LocalSessionPolicy, cfg map[string]any) ([]string, error) {
	current, err := policy.currentToolsets(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return slices.Clone(current), nil
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
