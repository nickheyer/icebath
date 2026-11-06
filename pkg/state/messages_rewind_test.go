package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func reactEmoji(s string) *string { return &s }

func TestCompactedReactionsAreDeliveredOnceWithoutRevivingRewoundRows(t *testing.T) {
	for _, reactBefore := range []bool{false, true} {
		dir := t.TempDir()
		t.Setenv("ICEBATH_HOME", dir)
		path := filepath.Join(dir, "state.db")
		db := usageOpen(t, path)
		ctx := context.Background()
		sid := "compacted-reactions"
		readSession(t, db, NewSession{ID: sid, Source: "test"})
		readAppend(t, db, sid, map[string]any{"role": "user", "content": "remember this answer"})
		target := readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "the original answer"})
		readAppend(t, db, sid, map[string]any{"role": "user", "content": "continue"})
		conv := archiveConversation(t, db, sid, false)
		tail := conv[len(conv)-1]
		if reactBefore {
			if _, _, err := db.SetMessageReaction(ctx, sid, target, reactEmoji("\U0001f44d"), "user"); err != nil {
				t.Fatal(err)
			}
		}
		archiveCompact(t, db, sid, []map[string]any{{"role": "user", "content": "summary"}, tail}, ArchiveCompactOptions{TailCount: 1})
		visible, _ := db.GetMessages(ctx, sid, GetMessagesOptions{IncludeCompacted: true})
		if !slices.ContainsFunc(visible, func(r *pyjson.Object) bool { return r.Vals["id"] == target }) {
			t.Fatal("archived target not visible")
		}
		if !reactBefore {
			if rs, ok, err := db.SetMessageReaction(ctx, sid, target, reactEmoji("\U0001f44d"), "user"); err != nil || !ok || len(rs) == 0 {
				t.Fatalf("reaction on archived row refused %v %v", ok, err)
			}
		}
		rewindTarget := readAppend(t, db, sid, map[string]any{"role": "user", "content": "discard this turn"})
		discarded := readAppend(t, db, sid, map[string]any{"role": "assistant", "content": "discarded answer"})
		if _, _, err := db.SetMessageReaction(ctx, sid, discarded, reactEmoji("\U0001f44e"), "user"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RewindToMessage(ctx, sid, rewindTarget, RewindOptions{}); err != nil {
			t.Fatal(err)
		}
		before := archiveConversation(t, db, sid, false)
		pending, err := db.TakeUnseenReactions(ctx, sid, "user")
		if err != nil || len(pending) != 1 || pending[0]["row_id"] != target || pending[0]["emoji"] != "\U0001f44d" || pending[0]["text"] != "the original answer" {
			t.Fatalf("pending %v %v", pending, err)
		}
		if again, _ := db.TakeUnseenReactions(ctx, sid, "user"); len(again) != 0 {
			t.Fatal("reaction delivered twice")
		}
		if after := archiveConversation(t, db, sid, false); !reflect.DeepEqual(after, before) {
			t.Fatal("taking reactions changed the conversation")
		}
		archived, _ := db.GetMessages(ctx, sid, GetMessagesOptions{IncludeCompacted: true})
		for _, r := range archived {
			if r.Vals["id"] == discarded {
				t.Fatal("rewound row revived")
			}
			if r.Vals["id"] == target && r.Vals["content"] != "the original answer" {
				t.Fatal("archived content changed")
			}
		}
		_ = db.Close()
		reopened := usageOpen(t, path)
		if again, _ := reopened.TakeUnseenReactions(ctx, sid, "user"); len(again) != 0 {
			t.Fatal("reaction redelivered after reopen")
		}
	}
}

func TestConversationRowIDsAreDurableAndOptIn(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "react-test", Source: "test"})
	readAppend(t, db, "react-test", map[string]any{"role": "user", "content": "how do i center a div"})
	readAppend(t, db, "react-test", map[string]any{"role": "assistant", "content": "use flexbox"})
	var rows []int64
	for _, m := range archiveConversation(t, db, "react-test", true) {
		id, ok := m["_row_id"].(int64)
		if !ok {
			t.Fatalf("row id %T", m["_row_id"])
		}
		rows = append(rows, id)
		for k := range m {
			if strings.HasPrefix(k, "_") && k != "_row_id" && k != DBPersistedMarker {
				t.Fatalf("private key %q reached the conversation", k)
			}
		}
	}
	if len(rows) != 2 || !slices.IsSorted(rows) || rows[0] == rows[1] {
		t.Fatalf("rows %v", rows)
	}
	for _, m := range archiveConversation(t, db, "react-test", false) {
		if _, has := m["_row_id"]; has || m[DBPersistedMarker] != true {
			t.Fatalf("default conversation shape %v", m)
		}
	}
}

func archivedCount(t *testing.T, db *SessionDB, sid string) (n int, compacted int) {
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if !pyTruthy(r.Vals["active"]) {
			n++
			if pyTruthy(r.Vals["compacted"]) {
				compacted++
			}
		}
	}
	return n, compacted
}

func dialogContents(t *testing.T, db *SessionDB, sid string) []any {
	var out []any
	for _, m := range archiveConversation(t, db, sid, false) {
		if m["role"] == "user" || m["role"] == "assistant" {
			out = append(out, m["content"])
		}
	}
	return out
}

func TestReplaceMessagesArchiveSiblings(t *testing.T) {
	ctx := context.Background()
	turns := func(texts ...string) []map[string]any {
		var out []map[string]any
		for i, text := range texts {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			out = append(out, map[string]any{"role": role, "content": text})
		}
		return out
	}
	replace := func(t *testing.T, db *SessionDB, sid string, msgs []map[string]any, o ReplaceOptions) {
		if err := db.ReplaceMessages(ctx, sid, msgs, o); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("persist_nonowned_branch_keeps_archived_rows", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "acp", Source: "test"})
		archiveBatch(t, db, "acp", turns("old question", "old answer", "another old question", "another old answer")...)
		archiveCompact(t, db, "acp", []map[string]any{{"role": "assistant", "content": "summary of old turns"}, {"role": "user", "content": "live question"}, {"role": "assistant", "content": "live answer"}}, ArchiveCompactOptions{})
		if n, _ := archivedCount(t, db, "acp"); n != 4 {
			t.Fatalf("archived %d", n)
		}
		replace(t, db, "acp", turns("rewritten", "rewritten answer"), ReplaceOptions{ActiveOnly: true})
		if n, _ := archivedCount(t, db, "acp"); n != 4 || !slices.Equal(dialogContents(t, db, "acp"), []any{"rewritten", "rewritten answer"}) {
			t.Fatalf("archived %d live %v", n, dialogContents(t, db, "acp"))
		}
	})
	t.Run("fresh_session_active_only_equals_full_replace", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "fresh", Source: "test"})
		archiveBatch(t, db, "fresh", turns("q", "a")...)
		replace(t, db, "fresh", turns("only"), ReplaceOptions{ActiveOnly: true})
		if n, _ := archivedCount(t, db, "fresh"); n != 0 || !slices.Equal(dialogContents(t, db, "fresh"), []any{"only"}) {
			t.Fatal("active only replace of an unarchived session differs from a full replace")
		}
	})
	t.Run("dropped_turns_survive_as_inactive_rows", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "drop", Source: "test"})
		archiveBatch(t, db, "drop", turns("first", "first reply", "second", "second reply")...)
		replace(t, db, "drop", turns("first", "first reply"), ReplaceOptions{ActiveOnly: true, ArchiveDropped: true})
		rows, _ := db.GetMessages(ctx, "drop", GetMessagesOptions{IncludeInactive: true})
		var recovered []any
		for _, r := range rows {
			if !pyTruthy(r.Vals["active"]) {
				recovered = append(recovered, r.Vals["content"])
			}
		}
		if !slices.Equal(dialogContents(t, db, "drop"), []any{"first", "first reply"}) || !slices.Contains(recovered, any("second")) || !slices.Contains(recovered, any("second reply")) {
			t.Fatalf("recovered %v", recovered)
		}
	})
	t.Run("archived_rows_use_rewind_marking_not_compaction", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "mark", Source: "test"})
		archiveBatch(t, db, "mark", turns("keep", "drop me")...)
		replace(t, db, "mark", turns("keep"), ReplaceOptions{ActiveOnly: true, ArchiveDropped: true})
		if n, compacted := archivedCount(t, db, "mark"); n == 0 || compacted != 0 {
			t.Fatalf("archived %d compacted %d", n, compacted)
		}
	})
	t.Run("default_stays_destructive", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "default", Source: "test"})
		archiveBatch(t, db, "default", turns("gone", "also gone")...)
		replace(t, db, "default", turns("fresh"), ReplaceOptions{})
		if n, _ := archivedCount(t, db, "default"); n != 0 {
			t.Fatalf("archived %d", n)
		}
	})
}

func markerDB(t *testing.T) (*SessionDB, string) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db := usageOpen(t, path)
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "do the full task"})
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "[memory]", "tool_calls": []any{map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "memory", "arguments": "{}"}}}})
	readAppend(t, db, "s1", map[string]any{"role": "tool", "content": "ok", "tool_call_id": "1"})
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "Here is the result."})
	return db, path
}

func backupSiblings(t *testing.T, path string) []string {
	matches, _ := filepath.Glob(path + ".pre-clean-markers-backup-*")
	return matches
}

func TestPurgeStaleToolCallMarkers(t *testing.T) {
	ctx := context.Background()
	t.Run("polluted_session_resumes_without_marker", func(t *testing.T) {
		db, _ := markerDB(t)
		contents := archiveContents(archiveConversation(t, db, "s1", false))
		if slices.Contains(contents, any("[memory]")) || !slices.Contains(contents, any("Here is the result.")) {
			t.Fatalf("contents %v", contents)
		}
	})
	t.Run("dry_run_reports_without_writing", func(t *testing.T) {
		db, path := markerDB(t)
		report, err := db.PurgeStaleToolCallMarkers(ctx, true, true)
		if err != nil || report.Vals["dry_run"] != true || report.Vals["rows_affected"] != 1 || report.Vals["backup_path"] != nil {
			t.Fatalf("report %v %v", report, err)
		}
		if raw := repairScalar(t, db, "SELECT content FROM messages WHERE role = 'assistant' AND tool_calls IS NOT NULL"); raw != "[memory]" || len(backupSiblings(t, path)) != 0 {
			t.Fatalf("dry run wrote %v", raw)
		}
	})
	t.Run("purge_clears_content_keeps_tool_calls", func(t *testing.T) {
		db, _ := markerDB(t)
		report, err := db.PurgeStaleToolCallMarkers(ctx, false, true)
		if err != nil || report.Vals["dry_run"] != false || report.Vals["rows_affected"] != 1 {
			t.Fatalf("report %v %v", report, err)
		}
		backup, _ := report.Vals["backup_path"].(string)
		if _, statErr := os.Stat(backup); backup == "" || statErr != nil {
			t.Fatalf("backup %q %v", backup, statErr)
		}
		if raw := repairScalar(t, db, "SELECT content FROM messages WHERE role = 'assistant' AND tool_calls IS NOT NULL"); raw != "" {
			t.Fatalf("content %v", raw)
		}
		if !pyTruthy(repairScalar(t, db, "SELECT tool_calls FROM messages WHERE role = 'assistant' AND tool_calls IS NOT NULL")) {
			t.Fatal("tool calls lost")
		}
		second, _ := db.PurgeStaleToolCallMarkers(ctx, false, true)
		if second.Vals["rows_affected"] != 0 || second.Vals["backup_path"] != nil {
			t.Fatalf("second %v", second)
		}
	})
	t.Run("no_backup_when_flag_false", func(t *testing.T) {
		db, path := markerDB(t)
		report, err := db.PurgeStaleToolCallMarkers(ctx, false, false)
		if err != nil || report.Vals["rows_affected"] != 1 || report.Vals["backup_path"] != nil || len(backupSiblings(t, path)) != 0 {
			t.Fatalf("report %v %v", report, err)
		}
	})
	t.Run("no_affected_rows_on_clean_db", func(t *testing.T) {
		db := usageDB(t)
		readSession(t, db, NewSession{ID: "s1", Source: "cli"})
		readAppend(t, db, "s1", map[string]any{"role": "user", "content": "What's the weather?"})
		readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "It's sunny."})
		report, err := db.PurgeStaleToolCallMarkers(ctx, false, true)
		if err != nil || report.Vals["rows_affected"] != 0 || len(report.Vals["row_ids"].([]any)) != 0 {
			t.Fatalf("report %v %v", report, err)
		}
		if got := archiveContents(archiveConversation(t, db, "s1", false)); !slices.Equal(got, []any{"What's the weather?", "It's sunny."}) {
			t.Fatalf("clean session %v", got)
		}
	})
}

func TestRewindToMessage(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "rw", Source: "cli"})
	u1 := readAppend(t, db, "rw", map[string]any{"role": "user", "content": "first"})
	a1 := readAppend(t, db, "rw", map[string]any{"role": "assistant", "content": "first reply"})
	u2 := readAppend(t, db, "rw", map[string]any{"role": "user", "content": "second"})
	readAppend(t, db, "rw", map[string]any{"role": "assistant", "content": "second reply"})
	var verr *stateValueError
	if _, err := db.RewindToMessage(ctx, "rw", a1, RewindOptions{}); !errors.As(err, &verr) || !strings.Contains(err.Error(), "role='assistant'") {
		t.Fatalf("assistant target %v", err)
	}
	if _, err := db.RewindToMessage(ctx, "rw", 9999, RewindOptions{}); !errors.As(err, &verr) {
		t.Fatalf("missing target %v", err)
	}
	if _, err := db.RewindToMessage(ctx, "rw", u2, RewindOptions{ExpectedActiveIDs: []int64{u1}}); err == nil || err.Error() != "active transcript changed before the rewind could be persisted" {
		t.Fatalf("stale active ids %v", err)
	}
	if _, err := db.RewindToMessage(ctx, "rw", u2, RewindOptions{ExpectedTargetContent: "edited"}); err == nil || err.Error() != "rewind target changed before it could be persisted" {
		t.Fatalf("stale target %v", err)
	}
	if _, err := db.RewindToMessage(ctx, "rw", u2, RewindOptions{PreserveCompactionHandoff: true}); !errors.As(err, &verr) || err.Error() != "preserve_compaction_handoff requires an active composite carrier" {
		t.Fatalf("handoff without carrier %v", err)
	}
	out, err := db.RewindToMessage(ctx, "rw", u2, RewindOptions{ExpectedTargetContent: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Keys, []string{"rewound_count", "target_message", "new_head_id"}) || out.Vals["rewound_count"] != 2 || out.Vals["new_head_id"] != a1 {
		t.Fatalf("result %v", out.Vals)
	}
	if target := out.Vals["target_message"].(*pyjson.Object); target.Vals["content"] != "second" || target.Vals["id"] != u2 {
		t.Fatalf("target %v", target.Vals)
	}
	if got := dialogContents(t, db, "rw"); !slices.Equal(got, []any{"first", "first reply"}) {
		t.Fatalf("live %v", got)
	}
	if repairScalar(t, db, "SELECT rewind_count FROM sessions WHERE id = 'rw'") != int64(1) || repairScalar(t, db, "SELECT message_count FROM sessions WHERE id = 'rw'") != int64(2) {
		t.Fatal("counters")
	}
	if _, err := db.RewindToMessage(ctx, "rw", u2, RewindOptions{ExpectedTargetContent: "second"}); !errors.As(err, &verr) || err.Error() != "rewind target is not active" {
		t.Fatalf("inactive target %v", err)
	}
}
