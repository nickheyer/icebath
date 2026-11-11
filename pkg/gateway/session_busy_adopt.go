package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
)

// True when the claimed turn runs an in process owner agent that latched steers reach, false on a refusal
func adoptsOwnerAgent(ctx context.Context, a *SessionAuthority, ref SessionRef) (bool, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return false, &pyKeyError{ref.SessionID}
	}
	adopts, err := ownerAgentPolicy(ctx, a, ref, live)
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return false, nil
	}
	return adopts, err
}

// True unless the session's policy is a cron job's or it executes in a managed worker
func ownerAgentPolicy(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) (bool, error) {
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return false, err
	}
	if policy != nil && policy.Source == "cron" {
		return false, nil
	}
	managed, err := managedPolicy(ctx, a, ref)
	return managed == nil, err
}
