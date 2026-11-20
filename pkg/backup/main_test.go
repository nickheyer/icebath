package backup

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Runs the database holder when re-executed as its child, the test suite otherwise
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if db := os.Getenv(dbHolderChildEnv); db != "" {
		holdDatabase(db)
	}
	os.Exit(m.Run())
}
