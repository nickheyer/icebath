package gatewaycli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

func TestHelpAndEmptyListHintNameTheResolvedHome(t *testing.T) {
	h := filepath.Join(t.TempDir(), "AppData", "Local", "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "API_SERVER_ENABLED"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	send, _, err := commands.NewCLI().Root.Find([]string{"send"})
	if err != nil {
		t.Fatal(err)
	}
	help := send.Long
	if !strings.Contains(help, filepath.Join(h, ".env")) || !strings.Contains(help, filepath.Join(h, "config.yaml")) {
		t.Fatalf("help %q", help)
	}
	if strings.Contains(help, "~/.icebath") {
		t.Fatalf("help names a fixed home: %q", help)
	}
	var out, errOut bytes.Buffer
	ns := map[string]any{"list_targets": true, "message": nil, "json": false, "quiet": false}
	code, err := gatewaycli.SendCommand(context.Background(), h, ns, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v %s", code, err, errOut.String())
	}
	if !strings.Contains(out.String(), filepath.Join(h, "channel_directory.json")) || strings.Contains(out.String(), "~/.icebath") {
		t.Fatalf("hint %q", out.String())
	}
}
