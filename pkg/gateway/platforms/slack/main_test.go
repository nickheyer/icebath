package slack

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the subcommand table double the process wiring would supply from the command registry
func TestMain(m *testing.M) {
	tools.ProfileSecrets = provider.ProfileSecretSource()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	SubcommandMap = func(context.Context) map[string]string {
		out := map[string]string{}
		for _, name := range []string{"new", "reset", "btw", "stop", "model", "reasoning", "compress", "help", "retry", "q", "queue"} {
			out[name] = "/" + name
		}
		return out
	}
	IsGatewayKnownCommand = func(ctx context.Context, name string) bool {
		return commands.IsGatewayKnownCommand(ctx, commands.Default, name)
	}
	AppManifest = func(ctx context.Context, cfg config.Tree) map[string]any {
		return commands.SlackAppManifest(ctx, commands.Default, cfg, "")
	}
	os.Exit(m.Run())
}
