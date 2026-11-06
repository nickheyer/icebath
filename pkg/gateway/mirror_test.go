package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeSessionsIndex(t *testing.T, h, body string) {
	dir := filepath.Join(h, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func strp(s string) *string { return &s }

func TestMirrorSessionsIndexLookup(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	writeSessionsIndex(t, h, `{"_README": "meta",
 "a": {"session_id": "old", "updated_at": "2026-01-01", "origin": {"platform": "Telegram", "chat_id": 42}},
 "b": {"session_id": "new", "updated_at": "2026-02-01", "origin": {"platform": "telegram", "chat_id": "42"}},
 "c": {"session_id": "thr", "updated_at": "2025-01-01", "origin": {"platform": "telegram", "chat_id": "42", "thread_id": "7"}},
 "d": {"session_id": "g1", "updated_at": "1", "origin": {"platform": "slack", "chat_id": "g", "user_id": "u1"}},
 "e": {"session_id": "g2", "updated_at": "2", "origin": {"platform": "slack", "chat_id": "g", "user_id": "u2"}}}`)
	cases := []struct {
		platform, chat string
		thread, user   *string
		want           string
	}{
		{"telegram", "42", nil, nil, "new"},
		{"telegram", "42", strp("7"), nil, "thr"},
		{"slack", "g", nil, strp("u1"), "g1"},
		{"slack", "g", nil, nil, ""},
		{"slack", "g", nil, strp("u9"), ""},
		{"discord", "42", nil, nil, ""},
	}
	for _, c := range cases {
		if got := findMirrorSessionID(ctx, c.platform, c.chat, c.thread, c.user); got != c.want {
			t.Errorf("%s:%s = %q want %q", c.platform, c.chat, got, c.want)
		}
	}
	if MirrorToSession(ctx, "discord", "42", "hi", MirrorOptions{}) {
		t.Fatal("mirrored without a session")
	}
	other := t.TempDir()
	writeSessionsIndex(t, other, `{"x": {"session_id": "pinned", "origin": {"platform": "telegram", "chat_id": "42"}}}`)
	mirrorSessionsIndex = filepath.Join(other, "sessions", "sessions.json")
	defer func() { mirrorSessionsIndex = "" }()
	if got := findMirrorSessionID(ctx, "telegram", "42", nil, nil); got != "pinned" {
		t.Fatalf("pinned index %q", got)
	}
}
