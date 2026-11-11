package gatewayrun

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == migrateGatewayStandInEntry {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	if dir := os.Getenv("ICEBATH_FAULTHANDLER_CHILD"); dir != "" {
		faulthandlerChild(dir)
		os.Exit(0)
	}
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry(context.Background())
	os.Exit(m.Run())
}
