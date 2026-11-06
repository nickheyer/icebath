package state

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func storedModelConfig(t *testing.T, db *SessionDB, sid string) map[string]any {
	t.Helper()
	session, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return ParseModelConfig(rowValue(session, "model_config"))
}

func TestPatchSessionModelConfigMergeAndDelete(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "PATCH_MODEL_CONFIG", Source: "cli", ModelConfig: map[string]any{"keep": "value", "drop": 1}})
	ctx := context.Background()
	if err := db.PatchSessionModelConfig(ctx, "PATCH_MODEL_CONFIG", map[string]any{"drop": nil, "added": 7}); err != nil {
		t.Fatal(err)
	}
	if got := storedModelConfig(t, db, "PATCH_MODEL_CONFIG"); !reflect.DeepEqual(got, map[string]any{"keep": "value", "added": int64(7)}) {
		t.Fatalf("merged %v", got)
	}
	if err := db.PatchSessionModelConfig(ctx, "NO_SUCH_SESSION", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PatchSessionModelConfig(ctx, "PATCH_MODEL_CONFIG", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := db.PatchModelConfig("NO_SUCH_SESSION", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if v, err := db.GetSessionModelConfigValue(ctx, "PATCH_MODEL_CONFIG", "added", nil); err != nil || v != int64(7) {
		t.Fatalf("value %v %v", v, err)
	}
	if v, _ := db.GetSessionModelConfigValue(ctx, "PATCH_MODEL_CONFIG", "absent", "dflt"); v != "dflt" {
		t.Fatalf("default %v", v)
	}
	if v, ok := db.ModelConfigValue("PATCH_MODEL_CONFIG", "keep"); !ok || v != "value" {
		t.Fatalf("agent read %v %v", v, ok)
	}
	if _, ok := db.ModelConfigValue("NO_SUCH_SESSION", "keep"); ok {
		t.Fatal("missing session reported a value")
	}
	if got := ParseModelConfig(`{"a": [1]}`); len(got) != 1 {
		t.Fatalf("parse %v", got)
	}
	for _, raw := range []any{"  ", "not json", 5, nil, "[1, 2]"} {
		if got := ParseModelConfig(raw); len(got) != 0 {
			t.Fatalf("parse %v -> %v", raw, got)
		}
	}
}

func TestArchiveModelConfigPatchRollsBackWithTranscript(t *testing.T) {
	db := usageDB(t)
	sid := "PRUNE_ATOMIC_ARCHIVE_FAILURE"
	readSession(t, db, NewSession{ID: sid, Source: "telegram", ModelConfig: map[string]any{"keep": "value", "_proactive_prune_rearm_tokens": 120000}})
	archiveBatch(t, db, sid, map[string]any{"role": "user", "content": "original"})
	profileExec(t, db, "CREATE TRIGGER fail_replacement BEFORE INSERT ON messages WHEN NEW.content = 'replacement' "+
		"BEGIN SELECT RAISE(ABORT, 'insert failed'); END")
	patch := pyjson.NewObject()
	patch.Set("_proactive_prune_rearm_tokens", nil)
	_, err := db.ArchiveAndCompact(context.Background(), sid, []map[string]any{{"role": "user", "content": "replacement"}},
		ArchiveCompactOptions{ModelConfigPatch: patch})
	if err == nil || !strings.Contains(err.Error(), "insert failed") {
		t.Fatalf("archive error %v", err)
	}
	conv, err := db.GetMessagesAsConversation(context.Background(), sid, ConversationOptions{})
	if err != nil || len(conv) == 0 || conv[0]["content"] != "original" {
		t.Fatalf("transcript %v %v", conv, err)
	}
	if got := storedModelConfig(t, db, sid); !reflect.DeepEqual(got, map[string]any{"keep": "value", "_proactive_prune_rearm_tokens": int64(120000)}) {
		t.Fatalf("model config %v", got)
	}
}
