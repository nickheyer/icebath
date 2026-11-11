package memory

import (
	"context"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Replaces the pm sync and the selected environment probe of dependency preparation until the returned call
func SwapSetupPM(sync func(context.Context, pm.SyncVenvOptions) error, running func(context.Context, string) (bool, string, error)) (restore func()) {
	prevSync, prevRunning := setupSyncVenv, setupRunningFromSelected
	setupSyncVenv, setupRunningFromSelected = sync, running
	return func() { setupSyncVenv, setupRunningFromSelected = prevSync, prevRunning }
}
