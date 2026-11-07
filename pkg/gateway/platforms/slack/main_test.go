package slack

import (
	"context"
	"os"
	"testing"
)

// Installs the subcommand table double the process wiring would supply from the command registry
func TestMain(m *testing.M) {
	SubcommandMap = func(context.Context) map[string]string {
		out := map[string]string{}
		for _, name := range []string{"new", "reset", "btw", "stop", "model", "reasoning", "compress", "help", "retry", "q", "queue"} {
			out[name] = "/" + name
		}
		return out
	}
	os.Exit(m.Run())
}
