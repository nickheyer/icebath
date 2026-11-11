package tuigateway

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Live WebSocket peer double carrying the identity the upgrade credential minted
type loginSocket struct {
	identity map[string]any
	closed   bool
}

func newLoginSocket(userID string) *loginSocket {
	return &loginSocket{identity: map[string]any{"provider": "basic", "user_id": userID}}
}

func (s *loginSocket) Write(frame *pyjson.Object) bool { return true }
func (s *loginSocket) Close()                          {}
func (s *loginSocket) AuthIdentity() map[string]any    { return s.identity }
func (s *loginSocket) Closed() bool                    { return s.closed }

// Record the way session.create stamps it, the login living on the record
func loginSession(t *testing.T, sid, key string, transport Transport) *Session {
	t.Helper()
	fields := map[string]any{"session_key": key, "transport": transport, "auth_user_id": nil}
	if userID := transportAuthUserID(transport); userID != nil {
		fields["auth_user_id"] = *userID
	}
	record := NewSession(fields)
	SetSessionRecord(sid, record)
	t.Cleanup(func() { SetSessionRecord(sid, nil) })
	return record
}

func TestAttachingADifferentLoginKeepsTheCreatorAndWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	creator, other := newLoginSocket("alice"), newLoginSocket("bob")
	record := loginSession(t, "sid-shared", "shared-key", creator)
	if !attachSessionTransport(record, other) || !attachSessionTransport(record, other) {
		t.Fatal("attach failed")
	}
	if userID := sessionAuthUserID(record); userID == nil || *userID != testHostOwner {
		t.Fatalf("auth user id = %v", userID)
	}
	if count := strings.Count(logs.String(), "keeps the user id"); count != 0 {
		t.Fatalf("two logins of the host owner logged as foreign %d times: %s", count, logs.String())
	}
}

func TestServerRequestFailsFastForAWsClientThatNeverAdvertised(t *testing.T) {
	ResetForTests()
	savedWrite, savedEmit, savedAnswerable, savedClients := currentSinks()
	BindSinks(savedWrite, savedEmit, sessionClientAnswersRequests, sessionAnsweringClients)
	t.Cleanup(func() {
		ResetForTests()
		BindSinks(savedWrite, savedEmit, savedAnswerable, savedClients)
	})
	ws := &recordingWS{}
	peer := NewWSTransport(ws, "test", nil)
	SetSessionRecord("ws-old", NewSession(map[string]any{"session_key": "ws-old", "transport": Transport(peer), "history": []any{}, "agent_ready": nil}))
	t.Cleanup(func() { SetSessionRecord("ws-old", nil) })
	started := time.Now()
	timeout := 5.0
	result, err := Send(context.Background(), "clarify", "ws-old",
		pyjson.ObjectOf("question", "q?", "choices", nil, "multi_select", false), &timeout, nil)
	if err != nil || result != nil {
		t.Fatalf("send = %v, %v", result, err)
	}
	if time.Since(started) >= time.Second {
		t.Fatalf("the refused send took %s", time.Since(started))
	}
	if sent := ws.lines(); len(sent) != 0 {
		t.Fatalf("frames written = %v", sent)
	}
	if open := OpenRequests("ws-old"); len(open) != 0 {
		t.Fatalf("open requests = %v", open)
	}
	settled := []*pyjson.Object{}
	if _, err := SendAsync("approval", "ws-old", pyjson.ObjectOf("request_id", "r1", "command", "rm", "description", "",
		"pattern_key", "", "pattern_keys", []any{}), func(result *pyjson.Object) { settled = append(settled, result) }); err != nil {
		t.Fatal(err)
	}
	if len(settled) != 1 || settled[0] != nil {
		t.Fatalf("settled = %v", settled)
	}
}
