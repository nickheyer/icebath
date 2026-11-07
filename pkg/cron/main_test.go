package cron

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Dispatches the scratch prune and external worker entries, a worker child running the real job path
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	SetupWorkerLogging = func(string, string) error { return nil }
	RegisterWorkerConfigHooks = func(context.Context, string, slog.Level) {}
	if len(os.Args) > 1 && os.Args[1] == ExternalWorkerEntry {
		runOneJobSeam = runOneJob
	}
	DispatchExternalWorkerEntry()
	os.Exit(m.Run())
}
