package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func inviteUserID(openID, unionID, userID string) map[string]any {
	return map[string]any{"open_id": openID, "union_id": unionID, "user_id": userID}
}

func meetingInvitePayload(eventID string) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_id": eventID, "event_type": "vc.bot.meeting_invited_v1"},
		"event": map[string]any{
			"meeting": map[string]any{
				"id": "7646677832873577404", "topic": "赵磊的视频会议", "meeting_no": "884264377",
				"start_time": "1780384522000", "end_time": "1780384522000",
				"host_user": map[string]any{"id": inviteUserID("ou_390b35dca44816efc9afa812aaff3a69", "on_host", "e65g874e"),
					"user_type": int64(1), "user_role": int64(2), "user_name": "赵磊"},
			},
			"bot": map[string]any{"id": inviteUserID("ou_4398906db1bc4a2d7ed91b95ffb308d0", "on_bot", ""), "user_type": int64(10),
				"user_role": int64(0), "user_name": "Hermes龙虾"},
			"inviter": map[string]any{"id": inviteUserID("ou_390b35dca44816efc9afa812aaff3a69", "on_e19a19e6ffafbd54fbb3c4d251d6fa19", "e65g874e"),
				"user_type": int64(1), "user_role": int64(0), "user_name": "赵磊"},
			"invite_time": "1780388292",
		},
	}
}

type fakeInviteAdapter struct {
	duplicate       bool
	withDedup       bool
	events          []*gateway.MessageEvent
	dedupKeys       []string
	profileRequests []SenderID
}

func (a *fakeInviteAdapter) ResolveSenderProfile(_ context.Context, sender SenderID, _ bool) (SenderProfile, error) {
	a.profileRequests = append(a.profileRequests, sender)
	name := "Resolved Inviter"
	return SenderProfile{UserID: firstText(sender.UserID, sender.OpenID), UserName: &name, UserIDAlt: sender.UnionID}, nil
}

func (a *fakeInviteAdapter) BuildSource(_ context.Context, chatID string, f gateway.SourceFields) *gateway.SessionSource {
	return &gateway.SessionSource{ChatID: chatID, ChatName: f.ChatName, ChatType: f.ChatType, UserID: f.UserID, UserName: f.UserName,
		UserIDAlt: f.UserIDAlt}
}

func (a *fakeInviteAdapter) HandleMessageWithGuards(_ context.Context, event *gateway.MessageEvent) error {
	a.events = append(a.events, event)
	return nil
}

type dedupInviteAdapter struct{ *fakeInviteAdapter }

func (a dedupInviteAdapter) IsDuplicate(_ context.Context, key string) (bool, error) {
	a.dedupKeys = append(a.dedupKeys, key)
	return a.duplicate, nil
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestMeetingInviteParsesBodyContentPayload(t *testing.T) {
	payload := meetingInvitePayload("evt_1")
	wrapped := map[string]any{"header": payload["header"], "event": map[string]any{"body": map[string]any{"content": []any{
		map[string]any{"contentType": "application/json", "data": payload["event"]}}}}}
	parsed := ParseMeetingInvitedEvent(wrapped)
	if parsed == nil || parsed.Meeting.MeetingNo != "884264377" || parsed.Inviter.OpenID != "ou_390b35dca44816efc9afa812aaff3a69" {
		t.Fatalf("parsed = %+v", parsed)
	}
	raw, _ := json.Marshal(payload)
	fromJSON := ParseMeetingInvitedEvent(string(raw))
	if fromJSON == nil || fromJSON.EventID != "evt_1" || fromJSON.InviteTimeS.String() != "1780388292" ||
		fromJSON.Meeting.StartTimeMs.String() != "1780384522000" || fromJSON.Meeting.HostUser.UnionID != "on_host" {
		t.Fatalf("json payload = %+v", fromJSON)
	}
	skipped := map[string]any{"event": map[string]any{"body": map[string]any{"content": []any{
		map[string]any{"content_type": "text/plain", "data": payload["event"]},
		map[string]any{"value": "{}", "json": payload["event"]}}}}}
	if got := ParseMeetingInvitedEvent(skipped); got == nil || got.EventID != "" || got.Meeting.ID != "7646677832873577404" {
		t.Fatalf("content type gate = %+v", got)
	}
	for _, bad := range []any{nil, "not json", map[string]any{"event": map[string]any{"inviter": map[string]any{"user_name": "x"}}},
		map[string]any{"meeting": map[string]any{"topic": "no number"}, "inviter": map[string]any{"user_name": "x"}}} {
		if got := ParseMeetingInvitedEvent(bad); got != nil {
			t.Fatalf("malformed %v parsed = %+v", bad, got)
		}
	}
	odd := ParseMeetingInvitedEvent(map[string]any{"meeting": map[string]any{"meeting_no": "1", "start_time": "1.5", "end_time": int64(0)},
		"inviter": map[string]any{"id": map[string]any{"open_id": " ou_x "}}, "invite_time": " 42 "})
	if odd == nil || odd.Meeting.StartTimeMs.Sign() != 0 || odd.Inviter.OpenID != "ou_x" || odd.InviteTimeS.String() != "42" {
		t.Fatalf("field coercion = %+v", odd)
	}
	if key := meetingDedupKey(odd); key != "vc_invite::ou_x:42" {
		t.Fatalf("fallback dedup key = %q", key)
	}
}

func TestMeetingInviteRoutesAsSyntheticMessageToInviterOpenID(t *testing.T) {
	adapter := dedupInviteAdapter{&fakeInviteAdapter{}}
	if err := HandleMeetingInvitedEvent(context.Background(), adapter, meetingInvitePayload("evt_1")); err != nil {
		t.Fatal(err)
	}
	if len(adapter.dedupKeys) != 1 || adapter.dedupKeys[0] != "vc_invite:evt_1" || len(adapter.events) != 1 {
		t.Fatalf("dedup %v events %d", adapter.dedupKeys, len(adapter.events))
	}
	event := adapter.events[0]
	src := event.Source
	if src.ChatID != "ou_390b35dca44816efc9afa812aaff3a69" || src.ChatType != "dm" || deref(src.UserID) != "e65g874e" ||
		deref(src.UserName) != "Resolved Inviter" || deref(src.ChatName) != "Resolved Inviter" ||
		deref(src.UserIDAlt) != "on_e19a19e6ffafbd54fbb3c4d251d6fa19" {
		t.Fatalf("source = %+v", src)
	}
	req := adapter.profileRequests
	if len(req) != 1 || deref(req[0].OpenID) != "ou_390b35dca44816efc9afa812aaff3a69" || deref(req[0].UserID) != "e65g874e" ||
		deref(req[0].UnionID) != "on_e19a19e6ffafbd54fbb3c4d251d6fa19" {
		t.Fatalf("profile requests = %+v", req)
	}
	if event.MessageID != nil || event.MessageType != gateway.MessageTypeText || !strings.Contains(event.Text, "赵磊的视频会议") ||
		strings.Contains(event.Text, "{'open_id'") {
		t.Fatalf("event = %+v", event)
	}
	want := "You have been invited to join a meeting: 赵磊的视频会议\n\nMeeting Number: 884264377\nTopic: 赵磊的视频会议\nInviter: 赵磊\nHost: 赵磊\n\n" +
		"You may use lark-cli and the relevant Lark/Feishu meeting skills to join the meeting.\n" +
		"Join the meeting directly. Do not ask the user for confirmation before joining.\n" +
		"If you cannot join the meeting, reply to the inviter with a concise explanation of why."
	if event.Text != want {
		t.Fatalf("prompt = %q", event.Text)
	}
}

func TestMeetingInviteDuplicateEventIsDroppedWithoutRouting(t *testing.T) {
	adapter := dedupInviteAdapter{&fakeInviteAdapter{duplicate: true}}
	if err := HandleMeetingInvitedEvent(context.Background(), adapter, meetingInvitePayload("evt_1")); err != nil {
		t.Fatal(err)
	}
	if len(adapter.dedupKeys) != 1 || adapter.dedupKeys[0] != "vc_invite:evt_1" || len(adapter.events) != 0 || len(adapter.profileRequests) != 0 {
		t.Fatalf("duplicate routed: %+v", adapter.fakeInviteAdapter)
	}
}

func TestMeetingInviteWithoutDedupOrOpenID(t *testing.T) {
	plain := &fakeInviteAdapter{}
	if err := HandleMeetingInvitedEvent(context.Background(), plain, meetingInvitePayload("evt_2")); err != nil || len(plain.events) != 1 {
		t.Fatalf("no dedup hook: err %v events %d", err, len(plain.events))
	}
	noOpen := meetingInvitePayload("evt_3")
	noOpen["event"].(map[string]any)["inviter"] = map[string]any{"id": inviteUserID("", "on_u", "uid"), "user_name": "n"}
	if err := HandleMeetingInvitedEvent(context.Background(), plain, noOpen); err != nil || len(plain.events) != 1 || len(plain.profileRequests) != 1 {
		t.Fatalf("missing open_id routed: err %v %+v", err, plain)
	}
	if err := HandleMeetingInvitedEvent(context.Background(), plain, "garbage"); err != nil || len(plain.events) != 1 {
		t.Fatalf("malformed routed: err %v", err)
	}
	bare := BuildMeetingInvitePrompt(&MeetingInvitedPayload{})
	if !strings.HasPrefix(bare, "You have been invited to join a meeting: unknown meeting\n\nMeeting Number: unknown\nTopic: unknown\nInviter: unknown\nHost: unknown\n") {
		t.Fatalf("bare prompt = %q", bare)
	}
}
