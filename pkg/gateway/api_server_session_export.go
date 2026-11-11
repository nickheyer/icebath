package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session a derived chat id names, a deleted one replaced by its next generation
func ImplicitAPISession(ctx context.Context, db *state.SessionDB, sessionID, requestID string) (string, error) {
	return implicitAPISession(ctx, db, sessionID, requestID)
}

// Live session a client addressed chain continues
func ContinuedAPISession(ctx context.Context, runner any, sessionID, requestID string) (string, error) {
	return continuedAPISession(ctx, runner, sessionID, requestID)
}
