package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
)

// The watchdog drain before its hard exit writes the records still queued for the log file
func TestWatchdogDrainWritesQueuedLogRecords(t *testing.T) {
	logging.ResetQueuedHandlers()
	t.Cleanup(logging.ResetQueuedHandlers)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if _, err := logging.SetupLogging(context.Background(), logging.Options{Home: h, Mode: "gateway", Force: true}); err != nil {
		t.Fatal(err)
	}
	logging.GetLogger("gateway.shutdown_watchdog").Warning("watchdog line before the hard exit")
	drainLogQueue(5.0)
	raw, err := os.ReadFile(filepath.Join(h, "logs", "agent.log"))
	if err != nil || !strings.Contains(string(raw), "watchdog line before the hard exit") {
		t.Fatalf("queued record lost %q %v", raw, err)
	}
}
