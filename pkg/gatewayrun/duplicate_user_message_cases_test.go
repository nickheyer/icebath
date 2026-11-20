package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/state"
)

// Telegram group source of the duplicate user message cases
func duplicateCaseSource() *gateway.SessionSource {
	user := "12345"
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001", ChatType: "group", UserID: &user}
}

// test_duplicate_user_message.py test_agent_failed_early_skip_db_when_agent_has_session_db
func TestAgentFailedEarlySkipDbWhenAgentHasSessionDb(t *testing.T) {
	server, err := fakellm.NewResponder(func(*fakellm.Request) any {
		return fakellm.Error{Status: 429, Message: "Too Many Requests"}
	}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r, _ := makeDispatchRunner(t, server)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := context.Background()
	source := duplicateCaseSource()
	key, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if key != "agent:main:telegram:group:-1001:12345" {
		t.Fatalf("session key = %q", key)
	}
	event := gateway.NewMessageEvent("hello world")
	messageID := "msg-42"
	event.Source, event.MessageID = duplicateCaseSource(), &messageID
	generation := r.beginSessionRunGeneration(key)
	response, err := r.handleMessageWithAgent(ctx, event, source, key, generation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a := r.cachedAgentFor(key, true); a != nil {
			r.cleanupAgentResources(context.Background(), a)
		}
	})
	if r.sessionDB(ctx) == nil {
		t.Fatal("the runner has no session database, the agent persists nothing")
	}
	if response == nil || !strings.Contains(*response, agent.FailedTurnNotice) {
		t.Fatalf("response = %v", response)
	}
	if strings.Contains(logs.String(), "Skipping duplicate user turn") {
		t.Fatalf("the user row append was skipped by the platform id guard: %s", logs.String())
	}
	entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := r.SessionDB(ctx).GetMessages(ctx, entry.SessionID, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var transcriptRows []map[string]any
	for _, obj := range stored {
		role, _ := obj.Get("role")
		content, _ := obj.Get("content")
		if role == "user" || role == "assistant" {
			transcriptRows = append(transcriptRows, map[string]any{"role": role, "content": content})
		}
	}
	if len(transcriptRows) != 2 || transcriptRows[0]["role"] != "user" || transcriptRows[1]["role"] != "assistant" {
		t.Fatalf("transcript rows = %#v", transcriptRows)
	}
	if transcriptRows[0]["content"] != "hello world" || transcriptRows[1]["content"] != agent.FailedTurnNotice {
		t.Fatalf("transcript rows = %#v", transcriptRows)
	}
	replay := append(transcriptRows, map[string]any{"role": "user", "content": "unrelated question"})
	repairs, err := state.RepairMessageSequence(&replay)
	if err != nil {
		t.Fatal(err)
	}
	if repairs != 0 || replay[len(replay)-1]["content"] != "unrelated question" {
		t.Fatalf("repairs = %d replay = %#v", repairs, replay)
	}
}
