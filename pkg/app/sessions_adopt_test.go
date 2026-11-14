package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/state"
)

var adoptOldChats = []struct{ sid, source, userID string }{
	{"old-desktop", "desktop", ""}, {"old-desktop-nous", "desktop", "nous:account"}, {"old-cli", "cli", ""},
	{"foreign-desktop", "desktop", "telegram:42"}, {"api-row", "api_server", ""},
}

// Seeded store in rollback journal mode where an exclusive writer lock also blocks readers
func adoptRollbackJournalStore(t *testing.T, h string) string {
	t.Helper()
	path := filepath.Join(h, "state.db")
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range adoptOldChats {
		sessionsTestSeed(t, db, state.NewSession{ID: c.sid, Source: c.source, Model: "old-model", Cwd: h, UserID: c.userID},
			strings.ToUpper(c.sid))
		if err := db.EndSession(c.sid, "session_switch"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	return path
}

func adoptStoreFiles(t *testing.T, h string) map[string]string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(h, "state.db*"))
	files := map[string]string{}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(m)] = string(b)
	}
	return files
}

func adoptRefused(t *testing.T, h string, dryRun bool) (int, string, string) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	notTTY := func() bool { return false }
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: notTTY, StdoutTTY: notTTY}, errOut)
	defer restore()
	code, err := CmdSessions(context.Background(), h, map[string]any{"sessions_action": "adopt", "dry_run": dryRun}, out, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

func TestAdoptNamesTheUpgradeForAStoreNoNewerHermesHasOpened(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		h := sessionsTestHome(t)
		path := adoptRollbackJournalStore(t, h)
		conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "DROP TABLE state_meta"); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		before := adoptStoreFiles(t, h)
		code, out, errText := adoptRefused(t, h, dryRun)
		if code != 1 || out != "" {
			t.Fatalf("status %d out %q", code, out)
		}
		if len(strings.Split(strings.TrimRight(errText, "\n"), "\n")) != 1 || !strings.Contains(errText, "older") ||
			!strings.Contains(errText, "`icebath sessions repair`") {
			t.Fatalf("err %q", errText)
		}
		if after := adoptStoreFiles(t, h); !mapsEqualText(before, after) {
			t.Fatal("store files changed")
		}
	}
}

func TestAdoptSaysToCloseTheHolderOfALockedStore(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		h := sessionsTestHome(t)
		path := adoptRollbackJournalStore(t, h)
		before := adoptStoreFiles(t, h)
		holder, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		code, out, errText := adoptRefused(t, h, dryRun)
		if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		holder.Close()
		if code != 1 || out != "" {
			t.Fatalf("status %d out %q", code, out)
		}
		if len(strings.Split(strings.TrimRight(errText, "\n"), "\n")) != 1 || !strings.Contains(errText, "locked") ||
			!strings.Contains(strings.ToLower(errText), "close") {
			t.Fatalf("err %q", errText)
		}
		if after := adoptStoreFiles(t, h); !mapsEqualText(before, after) {
			t.Fatal("store files changed")
		}
	}
}

func mapsEqualText(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
