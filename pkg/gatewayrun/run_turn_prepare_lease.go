package gatewayrun

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Serializes load, run and flush per session id, the session env restored when the wait times out
func (r *GatewayRunner) hmwaAcquireTurnLease(ctx context.Context, quickKey string, runGeneration int, entry *gateway.SessionEntry, sessionEnv *tools.Env) error {
	registry := r.turnLeases
	if registry == nil {
		return nil
	}
	timeout := settings.AgentGatewayTurnLeaseTimeout.Get(ctx).Seconds()
	token, err := registry.Acquire(ctx, entry.SessionID, quickKey, runGeneration, &timeout)
	if err != nil {
		var timedOut *TurnLeaseTimeoutError
		if errors.As(err, &timedOut) {
			r.clearSessionEnv(sessionEnv)
		}
		return err
	}
	if token != nil {
		r.turnLeaseTokens().Set(leaseTokenKey{quickKey, runGeneration}, token)
	}
	return nil
}
