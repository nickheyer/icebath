package pm

import "context"

// Stops an update that has no completion request, naming the old version handoff and exiting 1
func StopForRelaunchIncomplete(ctx context.Context) error { return stopForRelaunch(ctx, true) }
