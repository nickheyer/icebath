package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Texts of the completions a completer offers for a typed line
func completionTexts(c *CLI, text string) map[string]bool {
	out := map[string]bool{}
	for _, item := range cliSlashCompleter(c).Complete(text) {
		out[item.Text] = true
	}
	return out
}

func TestCliSlashCompleterFillsSkinsToolsetRowsAndConnectedPlatforms(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:test-token")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platform_toolsets:\n  cli:\n    - web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &CLI{ctx: context.Background()}
	if skins := completionTexts(c, "/skin "); !skins["default"] {
		t.Fatalf("/skin completions = %v", skins)
	}
	enable := completionTexts(c, "/tools enable ")
	if enable["web"] || !enable["terminal"] {
		t.Fatalf("/tools enable completions = %v", enable)
	}
	if disable := completionTexts(c, "/tools disable "); !disable["web"] {
		t.Fatalf("/tools disable completions = %v", disable)
	}
	if handoff := completionTexts(c, "/handoff "); !handoff["telegram"] {
		t.Fatalf("/handoff completions = %v", handoff)
	}
}
