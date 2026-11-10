package cron

import "context"

// Owner execution seam and the production tick and delivery seams, for the external test package of this directory
var (
	InstallCronOwner = installCronOwner
	UseRealTickSeams = useRealTickSeams
	UseRealDelivery  = useRealDelivery
)

// One synchronous scheduler tick over the store of a home
func TickSyncForHome(ctx context.Context, home string) (int, error) {
	return schedulerTick(ctx, openCronStore(home, nil), tickOptions{Verbose: false, Sync: true})
}

// Newest first ledger rows of one job in the store of a home
func ListExecutionsForHome(ctx context.Context, home, jobID string) ([]map[string]any, error) {
	return openExecutionsLedger(openCronStore(home, nil)).List(ctx, &jobID, 50, nil)
}
