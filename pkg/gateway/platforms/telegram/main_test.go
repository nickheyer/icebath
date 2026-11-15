package telegram

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Waits for the runtime status writes a test submitted so none lands in its removed home
func flushRuntimeStatus(t *testing.T) func() {
	return func() {
		if !gateway.FlushRuntimeStatus(30 * time.Second) {
			t.Error("gateway runtime status was still being written 30s after the test")
		}
	}
}

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	dir, err := os.MkdirTemp("", "icebath-telegram-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, flushRuntimeStatus)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
