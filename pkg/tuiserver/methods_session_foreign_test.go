package tuiserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// test_session_foreign.py test_foreign_rpc_preview_import_and_profile_isolation
func TestForeignRpcPreviewImportAndProfileIsolation(t *testing.T) {
	saved := tuiserver.ForeignSessions
	tuiserver.ForeignSessions.List, tuiserver.ForeignSessions.Preview, tuiserver.ForeignSessions.Import = app.ListForeignSessions, app.PreviewForeignSession, app.ImportBrowserSession
	t.Cleanup(func() { tuiserver.ForeignSessions = saved })
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "icebath-home"))
	t.Setenv("ICEBATH_DESKTOP", "")
	folder := filepath.Join(tmp, ".claude", "projects", "project")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, turn := range [][2]string{{"user", "# Files mentioned by the user:\nplan.md\n\n## My request:\nFix the import"},
		{"assistant", "Here is the fix."}, {"assistant", "And its test."}, {"user", "Continue"}} {
		line, _ := json.Marshal(map[string]any{"type": turn[0], "sessionId": "foreign-one", "cwd": tmp,
			"message": map[string]any{"role": turn[0], "content": turn[1]}})
		lines = append(lines, string(line))
	}
	logPath := filepath.Join(folder, "session.jsonl")
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(logPath)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "icebath-home", "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tuiserver.UseSharedDB(t, db)
	rpc := func(method string, params map[string]any) map[string]any {
		t.Helper()
		resp := tuiserver.LookupMethod("session.foreign."+method)(ctx, 1, params)
		if _, failed := resp["error"]; failed {
			t.Fatalf("%s %v", method, resp)
		}
		switch result := resp["result"].(type) {
		case map[string]any:
			return result
		case *pyjson.Object:
			return pyjson.Plain(result).(map[string]any)
		}
		t.Fatalf("%s result %#v", method, resp["result"])
		return nil
	}
	page := rpc("list", map[string]any{"limit": 1})
	row := page["sessions"].([]any)[0].(map[string]any)
	handle := row["id"]
	if row["title"] != "Fix the import" {
		t.Fatalf("title %v", row["title"])
	}
	if _, leaked := row["path"]; leaked {
		t.Fatal("the page exposed a path")
	}
	preview := rpc("preview", map[string]any{"id": handle})
	if preview["already_imported"] != nil {
		t.Fatalf("already imported %v", preview["already_imported"])
	}
	if count, _ := db.SessionCount(ctx, state.SessionCountOptions{}); count != 0 {
		t.Fatalf("preview wrote %d sessions", count)
	}
	results := make([]map[string]any, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = rpc("import", map[string]any{"id": handle}) })
	}
	wg.Wait()
	if results[0]["session_id"] != results[1]["session_id"] {
		t.Fatalf("concurrent imports %v %v", results[0], results[1])
	}
	sid, _ := results[0]["session_id"].(string)
	stored, err := db.GetSession(ctx, sid)
	if err != nil || stored == nil {
		t.Fatalf("stored %v %v", stored, err)
	}
	originJSON, _ := stored.Get("origin_json")
	var origin map[string]map[string]any
	if err := json.Unmarshal([]byte(originJSON.(string)), &origin); err != nil || origin["imported_from"]["foreign_session_id"] != "foreign-one" {
		t.Fatalf("origin %v %v", originJSON, err)
	}
	history, err := db.GetMessages(ctx, sid, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, message := range history {
		role, _ := message.Get("role")
		roles = append(roles, role.(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user"}) {
		t.Fatalf("roles %v", roles)
	}
	messageCount, _ := stored.Get("message_count")
	if total := preview["total"]; total != len(history) || pyjson.Plain(messageCount) != int64(len(history)) && messageCount != len(history) {
		t.Fatalf("history %d total %v message_count %v", len(history), total, messageCount)
	}
	if again := rpc("preview", map[string]any{"id": handle}); again["already_imported"] != sid {
		t.Fatalf("second preview %v", again["already_imported"])
	}
	if now, _ := os.ReadFile(logPath); !bytes.Equal(now, original) {
		t.Fatal("the foreign log changed")
	}
	other, err := state.NewSessionDB(ctx, filepath.Join(tmp, "other", "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tuiserver.UseSharedDB(t, other)
	if isolated := rpc("preview", map[string]any{"id": handle}); isolated["already_imported"] != nil {
		t.Fatalf("other store preview %v", isolated["already_imported"])
	}
	if fresh := rpc("import", map[string]any{"id": handle}); fresh["session_id"] == sid {
		t.Fatal("other store reused the session id")
	}
}
