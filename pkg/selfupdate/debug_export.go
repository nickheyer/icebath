package selfupdate

import "context"

// Deletes pending pastes whose expiry passed, returning how many were reaped and how many remain
func SweepExpiredPastes(ctx context.Context, now *float64) (int, int, error) {
	return debugSweepExpiredPastes(ctx, now)
}
