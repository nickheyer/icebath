package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Writes one codex rollout log whose only turn is a user message
func writeCodexRollout(t *testing.T, folder, name, text string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "rollout-"+name+".jsonl")
	if err := os.WriteFile(path, line, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Temp home with an empty codex session store
func codexStore(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "icebath-home"))
	folder := filepath.Join(tmp, ".codex", "sessions")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	return tmp, folder
}

// test_foreign_sessions_browser.py test_discovery_skips_inaccessible_log
func TestDiscoverySkipsInaccessibleLog(t *testing.T) {
	for _, operation := range []string{"resolve", "stat"} {
		t.Run(operation, func(t *testing.T) {
			_, folder := codexStore(t)
			writeCodexRollout(t, folder, "readable", "readable")
			inaccessible := filepath.Join(folder, "rollout-inaccessible.jsonl")
			if operation == "resolve" {
				if err := os.Symlink(inaccessible, inaccessible); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(filepath.Join(folder, "missing", "log.jsonl"), inaccessible); err != nil {
				t.Fatal(err)
			}
			page, err := ListForeignSessions("codex", 0, 25)
			if err != nil {
				t.Fatal(err)
			}
			var titles []string
			for _, row := range page["sessions"].([]any) {
				titles = append(titles, row.(map[string]any)["title"].(string))
			}
			if !reflect.DeepEqual(titles, []string{"readable"}) {
				t.Fatalf("titles %v", titles)
			}
		})
	}
}

// test_session_foreign.py test_foreign_pages_confine_handles_and_failed_import_rolls_back
func TestForeignPagesConfineHandlesAndFailedImportRollsBack(t *testing.T) {
	tmp, folder := codexStore(t)
	var paths []string
	for index := range 3 {
		path := writeCodexRollout(t, folder, fmt.Sprint(index), fmt.Sprintf("Question %d", index))
		stamp := time.Now().Add(time.Duration(index-3) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for _, older := range paths[:2] {
		if err := os.Chmod(older, 0o000); err != nil {
			t.Fatal(err)
		}
	}
	page, err := ListForeignSessions(nil, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page["unreadable"] != 0 || len(page["sessions"].([]any)) != 1 {
		t.Fatalf("the page parsed beyond its one row: %v", page)
	}
	for _, older := range paths[:2] {
		if err := os.Chmod(older, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nextPage, err := ListForeignSessions(nil, page["next_offset"], 1)
	if err != nil {
		t.Fatal(err)
	}
	first := page["sessions"].([]any)[0].(map[string]any)["id"]
	if second := nextPage["sessions"].([]any)[0].(map[string]any)["id"]; first == second {
		t.Fatal("pages repeated a handle")
	}
	for _, handle := range []string{filepath.Join(tmp, "secret.jsonl"), strings.Repeat("0", 64)} {
		if _, _, err := resolveForeignSession(handle); !isAppValueError(err) {
			t.Fatalf("handle %q resolved with %v", handle, err)
		}
	}
	ctx := context.Background()
	dbPath := filepath.Join(tmp, "icebath-home", "state.db")
	db, err := state.NewSessionDB(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TRIGGER interrupted_write AFTER INSERT ON messages BEGIN SELECT RAISE(ABORT, 'interrupted write'); END"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, err = ImportBrowserSession(ctx, first, db, "default")
	if err == nil || !strings.Contains(err.Error(), "interrupted write") {
		t.Fatalf("import error %v", err)
	}
	if count, err := db.SessionCount(ctx, state.SessionCountOptions{}); err != nil || count != 0 {
		t.Fatalf("sessions after a failed import %d %v", count, err)
	}
}

// Whether an error is the ValueError the browser raises
func isAppValueError(err error) bool {
	_, isValue := err.(*appValueError)
	return isValue
}
