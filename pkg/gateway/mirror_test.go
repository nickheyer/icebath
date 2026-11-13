package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
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
		if got, err := findMirrorSessionID(ctx, c.platform, c.chat, c.thread, c.user); err != nil || got != c.want {
			t.Errorf("%s:%s = %q, %v want %q", c.platform, c.chat, got, err, c.want)
		}
	}
	if MirrorToSession(ctx, "discord", "42", "hi", MirrorOptions{}) {
		t.Fatal("mirrored without a session")
	}
	other := t.TempDir()
	writeSessionsIndex(t, other, `{"x": {"session_id": "pinned", "origin": {"platform": "telegram", "chat_id": "42"}}}`)
	mirrorSessionsIndex = filepath.Join(other, "sessions", "sessions.json")
	defer func() { mirrorSessionsIndex = "" }()
	if got, err := findMirrorSessionID(ctx, "telegram", "42", nil, nil); err != nil || got != "pinned" {
		t.Fatalf("pinned index %q, %v", got, err)
	}
}

// Records the session each mirrored write targets and fails the write when failWith is set
func captureMirrorWrites(t *testing.T, failWith error) *[]string {
	t.Helper()
	var targets []string
	prev := appendMirrorMessage
	t.Cleanup(func() { appendMirrorMessage = prev })
	appendMirrorMessage = func(_ context.Context, sessionID, _, _ string) error {
		targets = append(targets, sessionID)
		return failWith
	}
	return &targets
}

func TestSuccessfulMirrorUsesUserIDForGroupSession(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeSessionsIndex(t, h, `{"alice": {"session_id": "sess_alice", "origin": {"platform": "telegram", "chat_id": "-1001", "user_id": "alice"}, "updated_at": "2026-01-01T00:00:00"},
 "bob": {"session_id": "sess_bob", "origin": {"platform": "telegram", "chat_id": "-1001", "user_id": "bob"}, "updated_at": "2026-02-01T00:00:00"}}`)
	targets := captureMirrorWrites(t, nil)
	if !MirrorToSession(context.Background(), "telegram", "-1001", "Hello group!", MirrorOptions{SourceLabel: "cli", UserID: strp("alice")}) {
		t.Fatal("group mirror with a user id failed")
	}
	if len(*targets) != 1 || (*targets)[0] != "sess_alice" {
		t.Fatalf("wrote to %v", *targets)
	}
}

func TestFailedSqliteWriteReportsFalse(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeSessionsIndex(t, h, `{"k": {"session_id": "sess_1", "origin": {"platform": "telegram", "chat_id": "12345"}, "updated_at": "2026-01-01T00:00:00"}}`)
	targets := captureMirrorWrites(t, errors.New("database is locked"))
	if MirrorToSession(context.Background(), "telegram", "12345", "Hello!", MirrorOptions{}) {
		t.Fatal("a failed transcript write reported success")
	}
	if len(*targets) != 1 || (*targets)[0] != "sess_1" {
		t.Fatalf("wrote to %v", *targets)
	}
}

func TestConnectionIsReleasedAfterUse(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var acquired, released []*state.DB
	oldAcquire, oldRelease := mirrorAcquireDB, mirrorReleaseDB
	t.Cleanup(func() { mirrorAcquireDB, mirrorReleaseDB = oldAcquire, oldRelease })
	mirrorAcquireDB = func(ctx context.Context, p string) (*state.DB, error) {
		db, err := oldAcquire(ctx, p)
		acquired = append(acquired, db)
		return db, err
	}
	mirrorReleaseDB = func(db *state.DB) {
		released = append(released, db)
		oldRelease(db)
	}
	ctx := context.Background()
	err := appendMirrorMessageSQLite(ctx, "sess_1", "assistant", "hello")
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("append against the acquired handle did not run once %v", err)
	}
	if len(acquired) != 1 || len(released) != 1 || released[0] != acquired[0] {
		t.Fatalf("acquired %v released %v", acquired, released)
	}
}
