package gateway

import (
	"context"
	"encoding/json"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Stateless adapter answering on a self post endpoint like the API server
type apiServerLikeAdapter struct {
	*baseFakeAdapter
	host, key, model string
	port             int
	db               *state.SessionDB
}

func (a *apiServerLikeAdapter) SelfPostTarget() (string, int, string, string) {
	return a.host, a.port, a.key, a.model
}

func (a *apiServerLikeAdapter) EnsureSessionDB(context.Context) (*state.SessionDB, error) {
	return a.db, nil
}

func (a *apiServerLikeAdapter) HandleMessage(context.Context, *MessageEvent) error {
	panic("non-push adapter must not receive handle_message wakes")
}

// Non push adapter bound to host and port with a key
func newAPIServerLike(host string, port int, key string) *apiServerLikeAdapter {
	a := &apiServerLikeAdapter{baseFakeAdapter: newBaseFake(PlatformAPIServer), host: host, port: port, key: key, model: "hermes"}
	a.Traits.SupportsAsyncDelivery = false
	return a
}

// Loopback server for the completions route and its port
func serveCompletions(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	n, _ := strconv.Atoi(port)
	return n
}

func TestAdapterSupportsPushDefaultTrue(t *testing.T) {
	if !AdapterSupportsPush(newBaseFake(PlatformTelegram)) {
		t.Fatal("default adapter is not push capable")
	}
	if AdapterSupportsPush(newAPIServerLike("0.0.0.0", 0, "test-key")) {
		t.Fatal("api server like adapter reads as push capable")
	}
}

func TestDeliverWakeNonPushSelfPostsRawSessionID(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]any{}
	port := serveCompletions(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen["session_id"], seen["auth"], seen["body"] = r.Header.Get("X-Icebath-Session-Id"), r.Header.Get("Authorization"), body
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}})
	})
	adapter := newAPIServerLike("0.0.0.0", port, "sekrit")
	if err := DeliverWake(context.Background(), adapter, WakeOptions{Text: "task done — wake", SessionID: "raw-sid-42"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["session_id"] != "raw-sid-42" || seen["auth"] != "Bearer sekrit" {
		t.Fatalf("headers %v", seen)
	}
	body := seen["body"].(map[string]any)
	if body["stream"] != false || !reflect.DeepEqual(body["messages"], []any{map[string]any{"role": "user", "content": "task done — wake"}}) {
		t.Fatalf("body %v", body)
	}
}

func TestDeliverWakeRetries429ThenSucceeds(t *testing.T) {
	previous := wakeRetryDelays
	wakeRetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { wakeRetryDelays = previous })
	var mu sync.Mutex
	calls := 0
	port := serveCompletions(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "busy"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	})
	if err := DeliverWake(context.Background(), newAPIServerLike("0.0.0.0", port, "test-key"), WakeOptions{Text: "x", SessionID: "sid"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestPersistDelegationDeliveryAppendsDeliveryRow(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sid := "raw-hq-sid"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "please confirm before writing"}, {"role": "assistant", "content": "awaiting confirmation", "finish_reason": "stop"}} {
		if _, err := db.AppendMessage(ctx, sid, m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	adapter := newAPIServerLike("0.0.0.0", 0, "test-key")
	adapter.db = db
	evt := map[string]any{"type": "async_delegation", "delegation_id": "deleg_x",
		"results": []any{map[string]any{"status": "completed"}, map[string]any{"status": "failed"}}, "total_duration_seconds": 12.5}
	if err := PersistDelegationDelivery(ctx, adapter, "[ASYNC DELEGATION BATCH COMPLETE — deleg_x]", sid, evt); err != nil {
		t.Fatal(err)
	}
	rows, err := db.GetMessages(ctx, sid, state.GetMessagesOptions{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows %d %v", len(rows), err)
	}
	delivery := rows[2]
	if role, _ := delivery.Get("role"); role != "user" {
		t.Fatalf("role %v", role)
	}
	if kind, _ := delivery.Get("display_kind"); kind != "async_delegation_complete" {
		t.Fatalf("display_kind %v", kind)
	}
	raw, _ := delivery.Get("display_metadata")
	items, ok := raw.(interface{ PyItems() iter.Seq2[any, any] })
	if !ok {
		t.Fatalf("display_metadata %T %v", raw, raw)
	}
	meta := map[string]any{}
	for k, v := range items.PyItems() {
		meta[k.(string)] = v
	}
	if meta["delegation_id"] != "deleg_x" || pyrepr.Str(meta["task_count"]) != "2" || pyrepr.Str(meta["failed_count"]) != "1" || pyrepr.Str(meta["duration_seconds"]) != "12.5" {
		t.Fatalf("metadata %v", meta)
	}
}

func TestPersistDelegationDeliveryRaisesWithoutDB(t *testing.T) {
	err := PersistDelegationDelivery(context.Background(), newAPIServerLike("0.0.0.0", 0, "test-key"), "x", "sid", nil)
	if err == nil || !strings.Contains(err.Error(), "SessionDB unavailable") {
		t.Fatalf("err %v", err)
	}
}
