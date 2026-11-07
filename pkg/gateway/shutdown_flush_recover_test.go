package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type appendCall struct {
	sessionID string
	fields    map[string]any
}

type recordingAppender struct {
	SessionStoreDB
	calls []appendCall
	fail  map[string]error
}

func (r *recordingAppender) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error) {
	if err := r.fail[sessionID]; err != nil {
		return 0, err
	}
	r.calls = append(r.calls, appendCall{sessionID, fields})
	return int64(len(r.calls)), nil
}

func writeFlushJSON(t *testing.T, path string, payload any) {
	t.Helper()
	text, err := pyjson.Dumps(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pendingPayload(key, text, sessionID string, ts int64) map[string]any {
	data := map[string]any{"text": text}
	if sessionID != "" {
		data["session_id"] = sessionID
	}
	return map[string]any{"session_key": key, "reason": "shutdown", "ts": ts, "data": data}
}

func TestRecoverInsertsViaAppendMessageAndDeletesFile(t *testing.T) {
	dir := useFlushDir(t)
	path := filepath.Join(dir, "test_session_123.json")
	writeFlushJSON(t, path, pendingPayload("agent:main:telegram:supergroup:123", "lost message", "20260728_120000_abc", 1785240000))
	db := &recordingAppender{}
	if n, err := RecoverPendingToDB(context.Background(), db, nil); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	want := []appendCall{{"20260728_120000_abc", map[string]any{"role": "user", "content": "lost message", "timestamp": int64(1785240000)}}}
	if !reflect.DeepEqual(db.calls, want) {
		t.Fatalf("calls %+v", db.calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("flush file kept")
	}
}

func TestRecoverPayloadWithoutSessionIDUsesResolverAndDeletesFile(t *testing.T) {
	dir := useFlushDir(t)
	path := filepath.Join(dir, "no_sid.json")
	const ts = int64(1785240000)
	writeFlushJSON(t, path, pendingPayload("agent:main:whatsapp:dm:15551234567", "lost message", "", ts))
	owned, routed := &recordingAppender{}, &recordingAppender{}
	var asked []any
	resolver := func(ctx context.Context, key string, notAfter *float64) (string, SessionStoreDB) {
		asked = append(asked, key, *notAfter)
		return "sid-resolved", routed
	}
	if n, err := RecoverPendingToDB(context.Background(), owned, resolver); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if !reflect.DeepEqual(asked, []any{"agent:main:whatsapp:dm:15551234567", float64(ts)}) {
		t.Fatalf("resolver asked %v", asked)
	}
	if len(routed.calls) != 1 || routed.calls[0].sessionID != "sid-resolved" || routed.calls[0].fields["content"] != "lost message" || len(owned.calls) != 0 {
		t.Fatalf("routed %+v owned %+v", routed.calls, owned.calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("flush file kept")
	}
	writeFlushJSON(t, path, pendingPayload("agent:main:whatsapp:dm:15551234567", "lost message", "", ts))
	none := func(ctx context.Context, key string, notAfter *float64) (string, SessionStoreDB) {
		return "sid-resolved", nil
	}
	if n, err := RecoverPendingToDB(context.Background(), owned, none); err != nil || n != 0 || len(owned.calls) != 0 {
		t.Fatalf("store-less resolution recovered %d %v %+v", n, err, owned.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("flush file not preserved")
	}
}

func TestRecoverSkipsFailingPayloadAndContinues(t *testing.T) {
	dir := useFlushDir(t)
	bad, good := filepath.Join(dir, "pending-a.json"), filepath.Join(dir, "pending-b.json")
	writeFlushJSON(t, bad, pendingPayload("key-bad", "first", "sid-bad", 1))
	writeFlushJSON(t, good, pendingPayload("key-good", "second", "sid-good", 2))
	db := &recordingAppender{fail: map[string]error{"sid-bad": errors.New("session is closed")}}
	if n, err := RecoverPendingToDB(context.Background(), db, nil); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if len(db.calls) != 1 || db.calls[0].sessionID != "sid-good" {
		t.Fatalf("calls %+v", db.calls)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal("good file kept")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("bad file lost")
	}
}

func TestFlushedOverflowIsReplayedByRecoverPendingToDB(t *testing.T) {
	dir := useFlushDir(t)
	if n, err := FlushOverflowToFile(context.Background(), map[string][]any{"agent:main:telegram:dm:1": {overflowEvent("orphan-1")}}, "shutdown"); err != nil || n != 1 {
		t.Fatalf("flushed %d %v", n, err)
	}
	db := &recordingAppender{}
	if n, err := RecoverPendingToDB(context.Background(), db, nil); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if len(db.calls) != 1 || db.calls[0].sessionID != "20260901_120000_fifo" || db.calls[0].fields["role"] != "user" || db.calls[0].fields["content"] != "orphan-1" {
		t.Fatalf("calls %+v", db.calls)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

func TestDrainTranscriptSpoolSkipsParseableNonDictPayload(t *testing.T) {
	dir := useFlushDir(t)
	for name, text := range map[string]string{"pending-00-scalar.json": "42", "pending-01-list.json": "[1, 2]"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if SpoolDroppedTranscriptMessage(context.Background(), "sess-1", map[string]any{"role": "user", "content": "hi"}) == "" {
		t.Fatal("spool write failed")
	}
	var replayed []map[string]any
	n, remaining, err := DrainTranscriptSpool(context.Background(), "sess-1", func(m map[string]any) error {
		replayed = append(replayed, m)
		return nil
	}, false)
	if err != nil || n != 1 || remaining != 0 {
		t.Fatalf("drain %d %d %v", n, remaining, err)
	}
	if !reflect.DeepEqual(replayed, []map[string]any{{"role": "user", "content": "hi"}}) {
		t.Fatalf("replayed %v", replayed)
	}
}
