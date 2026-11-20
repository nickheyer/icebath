package provider

import (
	"context"
	"fmt"
	"os"
)

const leftCoreAgentStartEnv = "ICEBATH_TEST_LEFT_CORE_AGENT_START"

// Runs a fresh process's agent start, printing every delivered notice, when the child env flag is set
func dispatchLeftCoreAgentStartEntry() {
	if os.Getenv(leftCoreAgentStartEnv) != "1" {
		return
	}
	RecoverLeftCoreAtStartup(context.Background(), func(message string) { fmt.Println("NOTICE", message) }, false)
	os.Exit(0)
}
