package mcp

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

const freshLaunchEnvChild = "ICEBATH_MCP_FRESH_LAUNCH_ENV_CHILD"

// Child process mode a fake subprocess runs as, sleep or exit with code and stderr text joined by colons
const fakeSubprocessChild = "ICEBATH_MCP_FAKE_SUBPROCESS_CHILD"

// Dispatches fake subprocess modes, installs the bridge state of a bridgeless process and pins fresh launch env children
func TestMain(m *testing.M) {
	if os.Getenv(connectorsLoadChild) != "" {
		os.Exit(0)
	}
	if mode := os.Getenv(fakeSubprocessChild); mode != "" {
		if mode == "sleep" {
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
		parts := strings.SplitN(mode, ":", 3)
		code, _ := strconv.Atoi(parts[len(parts)-2])
		os.Stderr.WriteString(parts[len(parts)-1])
		os.Exit(code)
	}
	if provider.GatewayHooks.BridgedAllowAllUsers == nil {
		provider.GatewayHooks.BridgedAllowAllUsers = func() (string, bool) { return "", false }
	}
	if os.Getenv(freshLaunchEnvChild) != "" {
		flag.Parse()
		if err := flag.Set("test.run", "^"+os.Getenv(freshLaunchEnvChild)+"$"); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}
