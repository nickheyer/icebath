package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins/pluginhost"
	"github.com/nickheyer/icebath/pkg/relayhost"
)

// Marks a re-executed test binary that runs the program entry with its own arguments
const runMainEnv = "ICEBATH_TEST_RUN_MAIN"

// Names the hermes fixture a re-executed test binary sets up before the program entry or runs in its place
const childModeEnv = "ICEBATH_TEST_CHILD_MODE"

// Comma list of customizations every re-executed test binary applies, the sitecustomize each hermes interpreter loads
const testSiteEnv = "ICEBATH_TEST_SITE"

// Fixture setups by name, each returns to the program entry or exits the process with its own status
var childModes = map[string]func(){}

// Process wide customizations by name, inherited by every child the process starts
var siteHooks = map[string]func(){}

// Runs the binary's own first step so a re-executed test binary dispatches child entries like the real one
func TestMain(m *testing.M) {
	pluginhost.DispatchPluginHostEntry(context.Background())
	relayhost.DispatchRelayHostEntry(context.Background())
	for _, name := range strings.FieldsFunc(os.Getenv(testSiteEnv), func(r rune) bool { return r == ',' }) {
		hook, known := siteHooks[name]
		if !known {
			fmt.Fprintf(os.Stderr, "unknown test site %q\n", name)
			os.Exit(2)
		}
		hook()
	}
	if name, set := os.LookupEnv(childModeEnv); set {
		mode, known := childModes[name]
		if !known {
			fmt.Fprintf(os.Stderr, "unknown child mode %q\n", name)
			os.Exit(2)
		}
		os.Unsetenv(childModeEnv)
		mode()
	}
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	startProcess(context.Background())
	if port, probing := os.LookupEnv(startServerProbeEnv); probing {
		os.Exit(runStartServerProbe(port))
	}
	os.Exit(runSandboxed(m))
}

// Runs the cases under a session home so work outliving a case never reaches the real home
func runSandboxed(m *testing.M) int {
	dir, err := os.MkdirTemp("", "icebath-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(dir)
	os.Setenv("ICEBATH_HOME", dir)
	return m.Run()
}
