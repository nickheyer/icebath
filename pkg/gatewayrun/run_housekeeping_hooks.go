package gatewayrun

import "context"

// Paste sweep of pkg/selfupdate deleting expired pending pastes, set by the command wiring
var SweepExpiredPastes func(ctx context.Context, now *float64) (deleted, remaining int, err error)
