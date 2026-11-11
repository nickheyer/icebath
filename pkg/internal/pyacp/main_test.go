package pyacp

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Blocks external HTTP then runs the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}
