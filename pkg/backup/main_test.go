package backup

import (
	"context"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"os"
	"testing"
)

const preflightChildEnv = "ICEBATH_BACKUP_TEST_PREFLIGHT_HOME"

// Runs the pre-flight entry when re-executed as its child, the test suite otherwise
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	if h := os.Getenv(preflightChildEnv); h != "" {
		os.Exit(PreflightStateDBMain(context.Background(), []string{h}, os.Stdout, os.Stderr))
	}
	if db := os.Getenv(dbHolderChildEnv); db != "" {
		holdDatabase(db)
	}
	os.Exit(m.Run())
}
