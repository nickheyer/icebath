package main

import (
	"context"
	"os"
	"testing"
)

// Runs the binary's own first step so a re-executed test binary dispatches child entries like the real one
func TestMain(m *testing.M) {
	startProcess(context.Background())
	os.Exit(m.Run())
}
