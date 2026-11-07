package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func useFlushDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pending_messages")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	patch(t, &shutdownFlushDir, func(context.Context) (string, error) { return dir, nil })
	return dir
}

func readFlushPayloads(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := pyjson.Loads(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pyjson.Plain(v).(map[string]any))
		if strings.Contains(filepath.Base(f), ":") || strings.Contains(filepath.Base(f), "telegram") {
			t.Fatalf("session key leaked into file name %s", f)
		}
	}
	return out
}

type mockPendingEvent struct{ attrs map[string]any }

func (m mockPendingEvent) pendingAttr(name string) (any, bool) {
	v, ok := m.attrs[name]
	return v, ok
}

func overflowEvent(text string) mockPendingEvent {
	return mockPendingEvent{map[string]any{"text": text, "session_id": "20260901_120000_fifo", "platform": "telegram",
		"sender_id": "1572286605", "sender_name": "tester", "reply_to": nil, "media": nil, "raw_event": nil}}
}

func TestShutdownFlushWritesStringPendingToFile(t *testing.T) {
	dir := useFlushDir(t)
	count, err := FlushPendingValuesToFile(context.Background(), map[string]any{"agent:main:telegram:supergroup:123": "hello world"}, "shutdown")
	if err != nil || count != 1 {
		t.Fatalf("count %d err %v", count, err)
	}
	payloads := readFlushPayloads(t, dir)
	if len(payloads) != 1 || payloads[0]["session_key"] != "agent:main:telegram:supergroup:123" || payloads[0]["reason"] != "shutdown" ||
		payloads[0]["data"].(map[string]any)["text"] != "hello world" {
		t.Fatalf("payloads %v", payloads)
	}
}

func TestShutdownFlushWritesMessageEventToFile(t *testing.T) {
	dir := useFlushDir(t)
	ev := mockPendingEvent{map[string]any{"text": "user message", "session_id": "20260728_120000_abc", "platform": "telegram",
		"sender_id": "456", "sender_name": "Alice", "reply_to": nil, "media": nil, "raw_event": nil}}
	count, err := FlushPendingValuesToFile(context.Background(), map[string]any{"session_key_1": ev}, "adapter_shutdown")
	if err != nil || count != 1 {
		t.Fatalf("count %d err %v", count, err)
	}
	data := readFlushPayloads(t, dir)[0]["data"].(map[string]any)
	want := map[string]any{"text": "user message", "session_id": "20260728_120000_abc", "platform": "telegram", "sender_id": "456", "sender_name": "Alice"}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("data %v", data)
	}
}

func TestShutdownFlushAdapterSlotsWriteEventText(t *testing.T) {
	dir := useFlushDir(t)
	count, err := FlushPendingToFile(context.Background(), map[string]*MessageEvent{"sk": {Text: "queued"}, "empty": nil}, "adapter_shutdown")
	if err != nil || count != 1 {
		t.Fatalf("count %d err %v", count, err)
	}
	payloads := readFlushPayloads(t, dir)
	if !reflect.DeepEqual(payloads[0]["data"], map[string]any{"text": "queued"}) || payloads[0]["session_key"] != "sk" {
		t.Fatalf("payloads %v", payloads)
	}
}

func TestShutdownFlushDirUsesTheActiveHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	dir, err := defaultShutdownFlushDir(context.Background())
	if err != nil || dir != filepath.Join(root, "pending_messages") {
		t.Fatalf("dir %q err %v", dir, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("flush dir mode %v %v", info, err)
	}
}

func TestShutdownFlushOverflowWritesOnePayloadPerEventInArrivalOrder(t *testing.T) {
	dir := useFlushDir(t)
	count, err := FlushOverflowToFile(context.Background(), map[string][]any{
		"agent:main:telegram:dm:1": {overflowEvent("follow-up B"), overflowEvent("follow-up C")},
		"agent:main:telegram:dm:2": {},
		"":                         {overflowEvent("keyless — skipped")},
	}, "shutdown")
	if err != nil || count != 2 {
		t.Fatalf("count %d err %v", count, err)
	}
	payloads := readFlushPayloads(t, dir)
	sort.Slice(payloads, func(i, j int) bool { return payloads[i]["seq"].(int64) < payloads[j]["seq"].(int64) })
	var texts []string
	for _, p := range payloads {
		texts = append(texts, p["data"].(map[string]any)["text"].(string))
		if p["session_key"] != "agent:main:telegram:dm:1" || p["reason"] != "shutdown" {
			t.Fatalf("payload %v", p)
		}
	}
	if !reflect.DeepEqual(texts, []string{"follow-up B", "follow-up C"}) {
		t.Fatal(texts)
	}
}

func TestShutdownFlushOverflowNoopOnEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if n, err := FlushOverflowToFile(ctx, map[string][]any{}, "shutdown"); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := FlushOverflowToFile(ctx, map[string][]any{"k": {}}, "shutdown"); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}
