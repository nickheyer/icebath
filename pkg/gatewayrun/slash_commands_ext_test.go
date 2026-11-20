package gatewayrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
)

// Installs the shared slash executor cmd/icebath wires at startup
func wireSlashExecutor(t *testing.T) {
	t.Helper()
	prev := gatewayrun.ExecuteSlashCommand
	t.Cleanup(func() { gatewayrun.ExecuteSlashCommand = prev })
	gatewayrun.ExecuteSlashCommand = func(ctx context.Context, name, surface, args string, options map[string]any) (gatewayrun.SlashCommandReply, error) {
		reply, err := commands.ExecuteCommand(ctx, name, commands.CommandContext{Surface: surface, Args: args, Options: options})
		return gatewayrun.SlashCommandReply{Text: reply.Text, Data: reply.Data, Format: reply.Format}, err
	}
}

func TestProfileCommandReportsSourceStampedProfile(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	milo := filepath.Join(h, "profiles", "milo")
	if err := os.MkdirAll(milo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(milo, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	wireSlashExecutor(t)
	ctx := context.Background()
	result, err := gatewayrun.ProfileCommandReply(ctx, "milo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "**Profile:** `milo`") {
		t.Fatalf("result %q", result)
	}
	expected := home.Display(home.Dir(gateway.ProfileRuntimeScope(ctx, milo, false)))
	if !strings.Contains(result, "**Home:** `"+expected+"`") {
		t.Fatalf("result %q want home %s", result, expected)
	}
}
