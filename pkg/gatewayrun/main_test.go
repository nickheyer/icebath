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
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	dir, err := os.MkdirTemp("", "gatewayrun-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
