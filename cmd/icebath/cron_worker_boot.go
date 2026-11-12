package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Import time boot of hermes_bootstrap for a cron worker, restoring, relaunching and activating as main does
func cronWorkerProcessBoot() {
	if restoreInterruptedPull() {
		relaunchAfterRestore()
	}
	pm.PinLauncherHome()
	bootLaunch()
	home.ExportScratchTmpEnv(context.Background())
}
