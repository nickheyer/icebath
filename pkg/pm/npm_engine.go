package pm

import "context"

// Stops an old updater before its retired npm engine retry path
func MaybeRepairNPMEngine(ctx context.Context) error { return stopForRelaunch(ctx, false) }
