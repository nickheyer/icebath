package backup

import (
	"context"
	"os"
	"testing"
)

const preflightChildEnv = "ICEBATH_BACKUP_TEST_PREFLIGHT_HOME"

// Runs the pre-flight entry when re-executed as its child, the test suite otherwise
func TestMain(m *testing.M) {
	if h := os.Getenv(preflightChildEnv); h != "" {
		os.Exit(PreflightStateDBMain(context.Background(), []string{h}, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
