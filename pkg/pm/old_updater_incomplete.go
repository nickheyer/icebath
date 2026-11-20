package pm

import (
	"context"
	"fmt"
)

// Stops an update that has no completion request, naming the old version handoff and exiting 1
func StopForRelaunchIncomplete(ctx context.Context) error {
	receipt, _ := currentUpdateHooks()
	if receipt.RecordStopReason != nil {
		receipt.RecordStopReason("old_version_handoff")
	}
	fmt.Fprintln(cliStderr(ctx), "You're updating from an older version of icebath. To complete this update, run `icebath update` again.")
	return &SystemExit{Code: 1}
}
