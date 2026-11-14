package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBreadcrumbWritesCompactPayloadInBuildOrder(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("TMUX_PANE", "%7")
	id := GetTerminalID()
	WriteBreadcrumb(context.Background(), "s1", "/work")
	raw, err := os.ReadFile(filepath.Join(h, "terminal-sessions", id))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, `{"session_id": "s1", "cwd": "/work", "ts": `) || !strings.HasSuffix(text, "}") || strings.Contains(text, "\n") {
		t.Fatalf("breadcrumb %q", text)
	}
}
