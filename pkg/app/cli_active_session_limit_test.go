package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
)

func TestCliClaimActiveSessionRespectsGlobalLimit(t *testing.T) {
	t.Setenv("COLUMNS", "500")
	rec := recordPrints(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("max_concurrent_sessions: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := TryAcquireActiveSession
	TryAcquireActiveSession = AcquireActiveSessionLease
	t.Cleanup(func() { TryAcquireActiveSession = original })
	cfg := map[string]any{"max_concurrent_sessions": 1}
	held, message, err := activesessions.TryAcquire(context.Background(), "held-session", "tui", cfg, nil, "", false)
	if err != nil || message != nil || held == nil {
		t.Fatalf("held %v message %v err %v", held, message, err)
	}
	c := bareCLI()
	c.console = console()
	c.sessionID = "new-cli-session"
	defer func() {
		_ = held.Release()
		c.releaseActiveSession()
	}()
	if c.claimActiveSession("cli", false) {
		t.Fatal("claim admitted past the global limit")
	}
	printed := rec.plain()
	if len(printed) != 1 || !strings.Contains(printed[0], "tui") {
		t.Fatalf("printed %q", printed)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if !c.claimActiveSession("cli", false) {
		t.Fatal("claim refused after the holder released")
	}
	entries, err := activesessions.RegistrySnapshot(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Value("session_id").(string))
	}
	if !slices.Equal(ids, []string{"new-cli-session"}) {
		t.Fatalf("snapshot %v", ids)
	}
}

func TestOneShotCliPreservesRefusalReason(t *testing.T) {
	refusal := &activesessions.Refusal{Message: "Ce chat est occupé.", Reason: activesessions.SessionNotOwned}
	original := TryAcquireActiveSession
	TryAcquireActiveSession = func(context.Context, string, string, map[string]any) (ActiveSessionLease, *activesessions.Refusal, error) {
		return nil, refusal, nil
	}
	t.Cleanup(func() { TryAcquireActiveSession = original })
	var stderr strings.Builder
	t.Cleanup(SetStreams(console(), &stderr))
	c := bareCLI()
	c.sessionID = "isolated"
	if c.claimActiveSession("cli", true) {
		t.Fatal("refused claim reported as held")
	}
	if !strings.Contains(stderr.String(), "icebath-refusal-reason: SESSION_NOT_OWNED\n") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
