package state

import (
	"context"
	"path/filepath"
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func chatPickParams(requestID string, payload any) *jsonutil.Object {
	return jsonutil.ObjectOf("request_id", requestID, "operation", "model", "payload", payload,
		"expected_revision", int64(0), "expected_generation", int64(0))
}

func TestChatPickKeyRefusesFencesThatAreNotNonNegativeInts(t *testing.T) {
	for name, params := range map[string]*jsonutil.Object{
		"missing generation": jsonutil.ObjectOf("request_id", "r", "operation", "model", "payload", jsonutil.ObjectOf(), "expected_revision", int64(0)),
		"bool revision":      jsonutil.ObjectOf("request_id", "r", "operation", "model", "payload", jsonutil.ObjectOf(), "expected_revision", true, "expected_generation", int64(0)),
		"negative":           jsonutil.ObjectOf("request_id", "r", "operation", "model", "payload", jsonutil.ObjectOf(), "expected_revision", int64(-1), "expected_generation", int64(0)),
		"empty request":      chatPickParams("", jsonutil.ObjectOf()),
		"list payload":       chatPickParams("r", []any{}),
	} {
		if _, _, err := ChatPickKey("telegram:1", "s", params); refusalReason(t, err) != "invalid_params" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	key, digest, err := ChatPickKey("telegram:1", "s", chatPickParams("r", jsonutil.ObjectOf("model", "m")))
	if err != nil {
		t.Fatal(err)
	}
	gen := int64(0)
	wantKey, wantDigest, err := MutationReceiptKey("telegram:1", "s", "r", "model", jsonutil.ObjectOf("model", "m"), 0, &gen)
	if err != nil || key != wantKey || digest != wantDigest {
		t.Fatalf("key %q %q want %q %q (%v)", key, digest, wantKey, wantDigest, err)
	}
}

func TestChatPickCommitsOnceAndReplaysTheExactRetry(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "INSERT INTO sessions(id,source,started_at) VALUES('chat','telegram',?)", now())
	params := chatPickParams("r", jsonutil.ObjectOf("model", "m"))
	key, digest, err := ChatPickKey("telegram:1", "chat", params)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := db.PrepareChatPick(ctx, epoch, key, digest, "chat", params)
	if err != nil {
		t.Fatal(err)
	}
	if row, has := prepared.Get("row"); !has || rowValue(row.(*jsonutil.Object), "id") != "chat" {
		t.Fatalf("prepare %v", prepared)
	}
	result := jsonutil.ObjectOf("operation", "model", "model", "m")
	committed, err := db.CommitChatPick(ctx, epoch, key, digest, "chat", params, result)
	if err != nil {
		t.Fatal(err)
	}
	if !anyutil.Equal(committed, jsonutil.ObjectOf("operation", "model", "model", "m", "revision", int64(1))) {
		t.Fatalf("committed %v", committed)
	}
	again, err := db.CommitChatPick(ctx, epoch, key, digest, "chat", params, jsonutil.ObjectOf("other", true))
	if err != nil || !anyutil.Equal(again, committed) {
		t.Fatalf("retry %v %v", again, err)
	}
	if got := storedValue(t, db, "SELECT runtime_revision FROM sessions WHERE id='chat'"); !anyutil.Equal(got, int64(1)) {
		t.Fatalf("revision %v", got)
	}
	prepared, err = db.PrepareChatPick(ctx, epoch, key, digest, "chat", params)
	if err != nil || !anyutil.Equal(rowValue(prepared, "receipt"), committed) {
		t.Fatalf("replayed prepare %v %v", prepared, err)
	}
	otherKey, otherDigest, err := ChatPickKey("telegram:1", "chat", chatPickParams("r", jsonutil.ObjectOf("model", "n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PrepareChatPick(ctx, epoch, otherKey, otherDigest, "chat", params); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	stale := chatPickParams("r2", jsonutil.ObjectOf("model", "m"))
	key2, digest2, err := ChatPickKey("telegram:1", "chat", stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitChatPick(ctx, epoch, key2, digest2, "chat", stale, result); refusalReason(t, err) != "revision_conflict" {
		t.Fatal(err)
	}
	if _, err := db.PrepareChatPick(ctx, epoch, key2, digest2, "missing", stale); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
}
