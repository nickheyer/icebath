package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/state"
)

func setLocalReceipt(ctx context.Context, t *testing.T, db *state.SessionDB, chatID, receipt string) {
	t.Helper()
	if err := db.SetMeta(ctx, state.LocalPolicyPrefix+chatID, receipt); err != nil {
		t.Fatal(err)
	}
}

// A LOCAL refresh keeps producer labels, folds interactive ones to the fallback and refuses a foreign session
func TestA2AStorageSourceKeepsTheOwnersCreationLabel(t *testing.T) {
	ctx, db := newTestStateDB(t)
	telegram := &SessionSource{Platform: PlatformTelegram, ChatID: "local-x"}
	if got, err := a2aStorageSource(ctx, db, telegram, "s1", "telegram"); err != nil || got != "telegram" {
		t.Fatalf("non local %q %v", got, err)
	}
	local := &SessionSource{Platform: PlatformLocal, ChatID: "local-a"}
	setLocalReceipt(ctx, t, db, "local-a", `{"session_id":"local-a","lineage":["local-a","child"],"policy":{"source":"a2a"}}`)
	if got, err := a2aStorageSource(ctx, db, local, "child", "local"); err != nil || got != "a2a" {
		t.Fatalf("producer label %q %v", got, err)
	}
	var refusal *state.RuntimeStoreError
	if _, err := a2aStorageSource(ctx, db, local, "other", "local"); !errors.As(err, &refusal) || refusal.Reason != "admission_conflict" {
		t.Fatalf("foreign session %v", err)
	}
	setLocalReceipt(ctx, t, db, "local-b", `{"session_id":"local-b","policy":{"source":"tui"}}`)
	if got, err := a2aStorageSource(ctx, db, &SessionSource{Platform: PlatformLocal, ChatID: "local-b"}, "local-b", "local"); err != nil || got != "local" {
		t.Fatalf("interactive label %q %v", got, err)
	}
	setLocalReceipt(ctx, t, db, "local-c", `{"session_id":"local-c","policy":{"source":"oneshot"}}`)
	if got, _ := a2aStorageSource(ctx, db, &SessionSource{Platform: PlatformLocal, ChatID: "local-c"}, "local-c", "local"); got != "oneshot" {
		t.Fatalf("kept stored label %q", got)
	}
	if _, err := a2aStorageSource(ctx, db, &SessionSource{Platform: PlatformLocal, ChatID: "local-missing"}, "x", "local"); !errors.As(err, &refusal) || refusal.Reason != "storage_unavailable" {
		t.Fatalf("missing receipt %v", err)
	}
}

// An adopted pre authority row keeps the label main wrote, an unadopted historical id keeps the fallback
func TestA2AStorageSourceAdoptedLegacyRowKeepsItsLabel(t *testing.T) {
	ctx, db := newTestStateDB(t)
	legacy := &SessionSource{Platform: PlatformLocal, ChatID: "20240101_old"}
	if got, err := a2aStorageSource(ctx, db, legacy, "20240101_old", "local"); err != nil || got != "local" {
		t.Fatalf("unadopted %q %v", got, err)
	}
	if err := db.CreateSession(state.NewSession{ID: "20240101_old", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, localLegacyPrefix+"20240101_old", "{}"); err != nil {
		t.Fatal(err)
	}
	setLocalReceipt(ctx, t, db, "20240101_old", `{"session_id":"20240101_old","legacy_session_id":"20240101_old","policy":{"source":"gui"}}`)
	if got, err := a2aStorageSource(ctx, db, legacy, "20240101_old", "local"); err != nil || got != "cli" {
		t.Fatalf("adopted %q %v", got, err)
	}
}

// The API binding names the stored label, every other platform and an unbound session keep the fallback
func TestAPIStorageSourceReadsTheBinding(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if got, err := apiStorageSource(ctx, db, "s1", "telegram"); err != nil || got != "telegram" {
		t.Fatalf("other platform %q %v", got, err)
	}
	if got, err := apiStorageSource(ctx, db, "s1", "api_server"); err != nil || got != "api_server" {
		t.Fatalf("unbound %q %v", got, err)
	}
	_ = db.SetMeta(ctx, state.LocalAPIBindingPrefix+"s1", `{"storage_source":"bot_room"}`)
	_ = db.SetMeta(ctx, state.LocalAPIBindingPrefix+"s2", `{"profile_id":"p"}`)
	_ = db.SetMeta(ctx, state.LocalAPIBindingPrefix+"s3", `not json`)
	if got, _ := apiStorageSource(ctx, db, "s1", "api_server"); got != "bot_room" {
		t.Fatalf("bound %q", got)
	}
	if got, _ := apiStorageSource(ctx, db, "s2", "api_server"); got != "api_server" {
		t.Fatalf("binding without label %q", got)
	}
	if _, err := apiStorageSource(ctx, db, "s3", "api_server"); err == nil {
		t.Fatal("a corrupt binding must raise")
	}
}

// A storage source refusal reaches the store caller the way hermes raises it
func TestUpdateSessionReturnsTheStorageSourceRefusal(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, &GatewayConfig{SessionsDir: dir})
	now := sessionNow()
	entry := NewSessionEntry("agent:main:local:dm:local-a", "stranger", now, now)
	entry.Platform, entry.ChatType = PlatformLocal, "dm"
	entry.Origin = &SessionSource{Platform: PlatformLocal, ChatID: "local-a", ChatType: "dm"}
	store.mu.Lock()
	store.entries.set(entry.SessionKey, entry)
	store.mu.Unlock()
	setLocalReceipt(ctx, t, db, "local-a", `{"session_id":"local-a","policy":{"source":"cli"}}`)
	var refusal *state.RuntimeStoreError
	if err := store.UpdateSession(ctx, entry.SessionKey, nil, false); !errors.As(err, &refusal) || refusal.Reason != "admission_conflict" {
		t.Fatalf("update session %v", err)
	}
}

// Generation ids and room ids match the hermes digests
func TestAPIGenerationAndRoomSessionIDs(t *testing.T) {
	if apiGenerationID("conv", 0) != "conv" || apiGenerationID("conv", 1) != "api-1430743e343c4433" {
		t.Fatalf("generation id %q", apiGenerationID("conv", 1))
	}
	identity := []string{"home", "room", "member", "work"}
	if roomSessionID(identity, 0) != "room_3303bbc220cc7503b44af32b8ef3736a" || roomSessionID(identity, 2) != "room_930f193827a1efc5393f915427b228f4" {
		t.Fatalf("room id %q %q", roomSessionID(identity, 0), roomSessionID(identity, 2))
	}
	ctx, db := newTestStateDB(t)
	if _, sid, err := hostedMemberSession(ctx, db, identity); err != nil || sid != roomSessionID(identity, 0) {
		t.Fatalf("hosted member %q %v", sid, err)
	}
	if sid, err := implicitAPISession(ctx, db, "conv", "req"); err != nil || sid != "conv" {
		t.Fatalf("implicit %q %v", sid, err)
	}
	if sid, ok, err := declaredAPISession(ctx, db, "named"); err != nil || ok || sid != "" {
		t.Fatalf("undeclared %q %v %v", sid, ok, err)
	}
	_ = db.SetMeta(ctx, state.LocalAPIDeclaredPrefix+"named", "sess-1")
	if sid, ok, _ := declaredAPISession(ctx, db, "named"); !ok || sid != "sess-1" {
		t.Fatalf("declared %q %v", sid, ok)
	}
}

// test_session_a2a.py test_forward_wakes_on_the_completion_frame_instead_of_a_fixed_100ms_poll
func TestForwardWakesOnTheCompletionFrameInsteadOfAFixedPoll(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time
	upgrader := websocket.Upgrader{Subprotocols: []string{CanonicalGatewayProtocol}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var writeMu sync.Mutex
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var request map[string]any
			_ = json.Unmarshal(raw, &request)
			mu.Lock()
			calls = append(calls, time.Now())
			n, done := len(calls), time.Since(calls[0]) >= time.Second
			mu.Unlock()
			status := "queued"
			if done {
				status = "terminal"
			}
			writeMu.Lock()
			_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{"status": status, "admission_id": "adm", "outcome": "completed"}})
			writeMu.Unlock()
			if n == 1 {
				go func() {
					time.Sleep(time.Second)
					writeMu.Lock()
					defer writeMu.Unlock()
					_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
						"type": "message.complete", "session_id": "s", "payload": map[string]any{"admission_id": "adm"}}})
				}()
			}
		}
	}))
	defer server.Close()
	endpoint := &GatewayEndpoint{APIOrigin: server.URL}
	oldEnsure, oldTicket := forwardEnsureRuntime, forwardSessionTicket
	forwardEnsureRuntime = func(context.Context, string) (ObservedEndpoint, error) {
		return ObservedEndpoint{State: "ready", Endpoint: endpoint}, nil
	}
	forwardSessionTicket = func(string, *GatewayEndpoint) (string, error) { return "fixture", nil }
	t.Cleanup(func() { forwardEnsureRuntime, forwardSessionTicket = oldEnsure, oldTicket })
	started := time.Now()
	receipt, err := ForwardToOwner(context.Background(), t.TempDir(), A2AForward{Agent: "dev", Peer: "alice", ContextID: "ctx", InputID: "i1", Text: "hello"}, 10*time.Second)
	elapsed := time.Since(started)
	if err != nil || receipt["status"] != "terminal" {
		t.Fatalf("receipt %v %v", receipt, err)
	}
	if elapsed >= 1600*time.Millisecond {
		t.Fatalf("woken late after %v", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) > 5 {
		t.Fatalf("%d a2a.forward RPCs for a one second turn", len(calls))
	}
}

// A runtime that is not ready refuses with its state and reason, an expired deadline ends the forward
func TestForwardToOwnerRefusesAnUnreadyOwnerAndHonoursTheTimeout(t *testing.T) {
	oldEnsure := forwardEnsureRuntime
	t.Cleanup(func() { forwardEnsureRuntime = oldEnsure })
	forwardEnsureRuntime = func(context.Context, string) (ObservedEndpoint, error) {
		return ObservedEndpoint{State: "pending"}, nil
	}
	var refusal *GatewayClientError
	if _, err := ForwardToOwner(context.Background(), t.TempDir(), A2AForward{}, time.Second); !errors.As(err, &refusal) || refusal.Msg != "gateway_pending:not_ready" {
		t.Fatalf("unready %v", err)
	}
	forwardEnsureRuntime = func(ctx context.Context, _ string) (ObservedEndpoint, error) {
		time.Sleep(300 * time.Millisecond)
		return ObservedEndpoint{}, nil
	}
	if _, err := ForwardToOwner(context.Background(), t.TempDir(), A2AForward{}, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout %v", err)
	}
}
