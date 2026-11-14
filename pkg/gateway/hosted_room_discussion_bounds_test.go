package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestWatermarkExcludesAMembersOldInputAndOwnReply(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "Old request.", "thread-1")
	first := hdSettleNext(t, room, db, "Old answer.")
	watermarks := hrOK(DeriveMemberWatermarks(room, hdEvents(t, db), hdLocalProfiles))
	var want int64
	for _, item := range hdEvents(t, db) {
		event := item.(map[string]any)
		if event["kind"] == "message.member" && event["payload"].(map[string]any)["task_id"] == first.Identity.TaskID {
			want = max(want, event["seq"].(int64))
		}
	}
	if watermarks[[2]string{"thread-1", first.Member.MemberID}] != want {
		t.Fatalf("watermarks %v want %d", watermarks, want)
	}
	latest := hdAppendUser(t, db, "user-2", "New request.", "thread-1")
	next := hdNextTask(t, room, db)
	prompt := next.Payload["prompt"].(string)
	if next.Member.Profile != "research" || next.Payload["source_event_seq"] != latest["seq"] || !strings.Contains(prompt, "New request.") ||
		strings.Contains(prompt, "Old request.") || strings.Contains(prompt, "Old answer.") {
		t.Fatalf("next %+v", next)
	}
}

func TestNewerSameThreadUserEventCancelsALateResult(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "First request.", "thread-1")
	stale := hdNextTask(t, room, db)
	latest := hdAppendUser(t, db, "user-2", "Second request.", "thread-1")
	publication := hdPublish(t, room, db, stale, "settled", map[string]any{"text": "Late stale answer."}, nil)
	if publication.TerminalKind != "turn.cancelled" || len(publication.Events) != 1 || publication.Events[0].Kind != "turn.cancelled" ||
		publication.Events[0].Payload["reason"] != "superseded_by_newer_user_event" {
		t.Fatalf("publication %+v", publication)
	}
	hdAppendPublication(t, db, publication)
	current := hdNextTask(t, room, db)
	if current.Payload["source_event_seq"] != latest["seq"] || !strings.Contains(current.Payload["prompt"].(string), "Second request.") {
		t.Fatalf("current %+v", current)
	}
}

func TestCrossThreadNewerUserDoesNotDiscardCompletedOldReply(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "First request.", "thread-1")
	old := hdNextTask(t, room, db)
	hdAppendUser(t, db, "user-2", "Other topic.", "thread-2")
	publication := hdPublish(t, room, db, old, "settled", map[string]any{"text": "Completed first topic."}, nil)
	if len(publication.Events) != 2 || publication.Events[0].Kind != "message.member" || publication.Events[1].Kind != "turn.settled" {
		t.Fatalf("publication %+v", publication)
	}
}

func TestOversizedMemberReplyIsTruncatedAndNextTurnStaysServiceable(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-large", strings.Repeat("u", maxUserTextBytes), "thread-1")
	first := hdNextTask(t, room, db)
	publication := hdPublish(t, room, db, first, "settled", map[string]any{"text": strings.Repeat("é", maxMemberTextBytes+100)}, nil)
	var memberText string
	for _, event := range publication.Events {
		if event.Kind == "message.member" {
			memberText = event.Payload["text"].(string)
			break
		}
	}
	if len(memberText) > maxMemberTextBytes || !strings.HasSuffix(memberText, "share the full result as a file.]") {
		t.Fatalf("member text of %d bytes", len(memberText))
	}
	hdAppendPublication(t, db, publication)
	followup := hdNextTask(t, room, db).Payload["prompt"].(string)
	if len(followup) > driverMaxPromptBytes || !strings.Contains(followup, "Earlier content omitted") {
		t.Fatalf("followup of %d bytes", len(followup))
	}
}

func TestThreeRoundBound(t *testing.T) {
	db, room := hdRoomDB(t)
	room["members"] = hdMembers[:2]
	hdAppendUser(t, db, "user-1", "Discuss.", "thread-1")
	for index := range MaxDiscussionRounds * 2 {
		task := hdNextTask(t, room, db)
		peer := "research"
		if task.Member.Profile == "research" {
			peer = "build"
		}
		hdAppendPublication(t, db, hdPublish(t, room, db, task, "settled", map[string]any{"text": fmt.Sprintf("Reply %d. @%s", index, peer)}, nil))
	}
	if decision := hdDecide(t, room, db); decision.Status != "bounded" || decision.Reason != "max_rounds" {
		t.Fatalf("decision %+v", decision)
	}
}

func TestTenMessageBound(t *testing.T) {
	db := hrDB(t)
	var members []any
	for _, profile := range hdLocalProfiles {
		members = append(members, map[string]any{"member_id": "member-" + profile, "profile": profile, "handle": profile})
	}
	room := hrOK(CreateRoom(context.Background(), db, CreateRoomInput{RoomID: "room-1", Name: "Large", Members: members, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(1)}))
	hdAppendUser(t, db, "user-1", "Discuss.", "thread-1")
	for index := range MaxDiscussionMessages {
		hdSettleNext(t, room, db, fmt.Sprintf("Reply %d. @everyone", index))
	}
	if decision := hdDecide(t, room, db); decision.Status != "bounded" || decision.Reason != "max_messages" {
		t.Fatalf("decision %+v", decision)
	}
}
