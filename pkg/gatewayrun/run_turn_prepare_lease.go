package gatewayrun

import (
	"context"
	"errors"
	"os"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Env var as a float, the default when unset, empty or malformed
func floatEnv(name string, def float64) float64 {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return def
	}
	value, err := pyint.ParseFloat(raw)
	if err != nil {
		return def
	}
	return value
}

// Serializes load, run and flush per session id, the session env restored when the wait times out
func (r *GatewayRunner) hmwaAcquireTurnLease(ctx context.Context, quickKey string, runGeneration int, entry *gateway.SessionEntry, sessionEnv *tools.Env) error {
	registry := r.turnLeases
	if registry == nil {
		return nil
	}
	timeout := floatEnv("ICEBATH_TURN_LEASE_TIMEOUT", defaultLeaseWait)
	token, err := registry.Acquire(ctx, entry.SessionID, quickKey, runGeneration, &timeout)
	if err != nil {
		var timedOut *TurnLeaseTimeoutError
		if errors.As(err, &timedOut) {
			r.clearSessionEnv(sessionEnv)
		}
		return err
	}
	if token != nil {
		turn := &r.sessionState(quickKey).turn
		if turn.leaseTokens == nil {
			turn.leaseTokens = map[int]*TurnLeaseToken{}
		}
		turn.leaseTokens[runGeneration] = token
	}
	return nil
}
