package acp

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/state"
)

// Blocks external HTTP, installs the wiring hooks cmd/icebath assigns, then runs the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	NewSurfaceLabelReader = func(db *state.SessionDB) RichSessionLister { return gatewaycli.NewSurfaceLabelReader(db) }
	os.Exit(m.Run())
}
