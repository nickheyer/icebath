package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSecretOfARoutedTaskUnderMultiplexSemanticsNeverReadsTheLaunchValue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	routed := t.TempDir()
	if err := os.WriteFile(filepath.Join(routed, ".env"), []byte("OTHER_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_SERVICE_TOKEN", "launch-value")
	ctx := provider.WithMultiplexContext(context.Background(), true)
	value, found, err := getSecret(ctx, &tools.Env{Home: routed, Config: config.Tree{}, Secrets: config.NewSecrets(routed)}, "MCP_SERVICE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("routed task under multiplex semantics read the launch process value %q", value)
	}
}
