package main

import (
	"context"
	"os"
	"testing"
)

// Marks a re-executed test binary that runs the program entry with its own arguments
const runMainEnv = "ICEBATH_TEST_RUN_MAIN"

// Runs the binary's own first step so a re-executed test binary dispatches child entries like the real one
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	startProcess(context.Background())
	if port, probing := os.LookupEnv(startServerProbeEnv); probing {
		os.Exit(runStartServerProbe(port))
	}
	os.Exit(m.Run())
}
