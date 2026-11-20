package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func mustJSONObject(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func TestBuildFromSessionsBuildsFromSessionsJson(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	p := filepath.Join(h, "sessions", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"session_1": {"origin": {"platform": "telegram", "chat_id": "12345", "chat_name": "Alice"}, "chat_type": "dm"},
		"session_2": {"origin": {"platform": "telegram", "chat_id": "67890", "user_name": "Bob"}, "chat_type": "group"},
		"session_3": {"origin": {"platform": "discord", "chat_id": "99999"}}}`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := BuildFromSessions(context.Background(), "telegram")
	names := map[any]bool{}
	for _, e := range entries {
		n, _ := e.Get("name")
		names[n] = true
	}
	if len(entries) != 2 || !names["Alice"] || !names["Bob"] {
		t.Fatalf("entries=%v", entries)
	}
}

func TestFormatDirectoryForDisplayPlatformWithNoChannelsGetsHint(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, err := FormatDirectoryForDisplay(context.Background(), mustJSONObject(t, `{"simplex": [], "telegram": [{"id": "1", "name": "home", "type": "dm"}]}`))
	if err != nil || !strings.Contains(out, "Simplex:") || !strings.Contains(out, "telegram:home") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFormatDirectoryForDisplayExplicitPlatformsOverrideDisk(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, err := FormatDirectoryForDisplay(context.Background(), mustJSONObject(t, `{"irc": [{"id": "#chan", "name": "#chan", "type": "channel"}]}`))
	if err != nil || !strings.Contains(out, "irc:#chan") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
