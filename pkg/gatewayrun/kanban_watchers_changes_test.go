package gatewayrun

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/kanban"
)

// Task with a changes requested event and a Telegram topic subscription in the mode
func createReviewBlock(t *testing.T, mode, reason string) string {
	t.Helper()
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "existing implementation card", "implementer", kstr("agent:main:telegram:thread:chat-1:topic-7"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", ThreadID: kstr("topic-7"),
		ChatType: kstr("thread"), DeliveryMode: kstr(mode), DeliveryMetadata: map[string]any{"thread_id": "topic-7", "chat_type": "thread"}})
	kanbanAppend(t, conn, tid, "changes_requested", kobj("reason", reason, "reviewer", "claude-qa", "implementer", "codex-cua", "status", "ready"))
	return tid
}

// Unseen changes requested events of the topic subscription
func unseenChanges(t *testing.T, tid string) []*kanban.Event {
	t.Helper()
	conn := kanbanTestConn(t)
	_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, "telegram", "chat-1", kstr("topic-7"), []string{"changes_requested"})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestChangesRequestedNotifyWakeIsActionableAndExactlyRouted(t *testing.T) {
	kanbanTestDB(t, "review-block.db")
	tid := createReviewBlock(t, "notify+wake", "Tests need updates")
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 {
		t.Fatalf("sent %d", len(adapter.sent))
	}
	text := adapter.sent[0].text
	if !strings.HasPrefix(text, "🛑 [default] Kanban "+tid+" review requested changes/BLOCK: Tests need updates") ||
		!strings.Contains(text, "reviewer @claude-qa → implementer @codex-cua") {
		t.Fatalf("text %q", text)
	}
	if adapter.sent[0].metadata["thread_id"] != "topic-7" || len(adapter.handled) != 1 {
		t.Fatalf("metadata %v handled %d", adapter.sent[0].metadata, len(adapter.handled))
	}
	wake := adapter.handled[0]
	if wake.Source.ChatID != "chat-1" || wake.Source.ChatType != "thread" || *wake.Source.ThreadID != "topic-7" {
		t.Fatalf("source %+v", wake.Source)
	}
	for _, want := range []string{"implementation is not approved", "Inspect the existing card and its current review run", "do not create a duplicate task"} {
		if !strings.Contains(wake.Text, want) {
			t.Fatalf("wake lacks %q: %q", want, wake.Text)
		}
	}
	if strings.Count(wake.Text, "do not create a duplicate task") != 1 || len(unseenChanges(t, tid)) != 0 {
		t.Fatalf("wake %q", wake.Text)
	}
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || len(adapter.handled) != 1 {
		t.Fatalf("replay sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
}

func TestChangesRequestedNotifyIsPassiveOnly(t *testing.T) {
	kanbanTestDB(t, "notify.db")
	tid := createReviewBlock(t, "notify", "Tests need updates")
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || len(adapter.handled) != 0 || len(unseenChanges(t, tid)) != 0 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
}

func TestChangesRequestedWakeOnlyHasNoPassivePost(t *testing.T) {
	kanbanTestDB(t, "wake.db")
	tid := createReviewBlock(t, "wake", "Tests need updates")
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 0 || len(adapter.handled) != 1 || len(unseenChanges(t, tid)) != 0 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
}

func TestChangesRequestedSendFailureRetriesWithoutEventLoss(t *testing.T) {
	kanbanTestDB(t, "retry.db")
	tid := createReviewBlock(t, "notify", "Tests need updates")
	failing := newKanbanRecordingAdapter()
	failing.failSend = true
	runOneNotifierTick(t, makeKanbanRunner(t, failing))
	if len(failing.sent) != 1 || len(unseenChanges(t, tid)) != 1 {
		t.Fatalf("failing sent %d", len(failing.sent))
	}
	healthy := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, healthy))
	if len(healthy.sent) != 1 || len(unseenChanges(t, tid)) != 0 {
		t.Fatalf("healthy sent %d", len(healthy.sent))
	}
}

func TestChangesRequestedReasonIsRedactedPathSafeAndTruncated(t *testing.T) {
	kanbanTestDB(t, "redact.db")
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	reason := "See /Users/alice/private/review.log token=" + secret + " " + strings.Repeat("x", 300)
	createReviewBlock(t, "notify", reason)
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	text := adapter.sent[0].text
	if strings.Contains(text, "/Users/alice") || strings.Contains(text, "abcdefghijklmnopqrstuvwxyz") ||
		!strings.Contains(text, "[local path]") || !strings.Contains(text, "… — reviewer @claude-qa") {
		t.Fatalf("text %q", text)
	}
}
