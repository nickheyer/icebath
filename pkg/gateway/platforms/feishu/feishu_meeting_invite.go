package feishu

import (
	"context"
	"log/slog"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type MeetingInviteUser struct {
	OpenID   string
	UserID   string
	UnionID  string
	UserName string
}

type MeetingInviteMeeting struct {
	ID          string
	Topic       string
	MeetingNo   string
	StartTimeMs *big.Int
	EndTimeMs   *big.Int
	HostUser    *MeetingInviteUser
}

type MeetingInvitedPayload struct {
	EventID     string
	Meeting     *MeetingInviteMeeting
	Inviter     *MeetingInviteUser
	InviteTimeS *big.Int
}

// Feishu ID tiers of a sender the adapter resolves to a profile, absent tiers nil
type SenderID struct {
	OpenID  *string
	UserID  *string
	UnionID *string
}

// Session source fields the adapter resolves for a sender
type SenderProfile struct {
	UserID    *string
	UserName  *string
	UserIDAlt *string
}

// Adapter surface the meeting invite handler routes the synthetic event through
type MeetingInviteHost interface {
	ResolveSenderProfile(ctx context.Context, sender SenderID, isBot bool) (SenderProfile, error)
	BuildSource(ctx context.Context, chatID string, f gateway.SourceFields) *gateway.SessionSource
	HandleMessageWithGuards(ctx context.Context, event *gateway.MessageEvent) error
}

// Adapter dedup check the handler consults when the adapter provides one
type DuplicateChecker interface {
	IsDuplicate(ctx context.Context, key string) (bool, error)
}

// Payload dict of a mapping or JSON string value, empty for anything else
func asDict(value any) map[string]any {
	if s, ok := value.(string); ok {
		parsed, err := pyjson.Loads(s)
		if err != nil {
			return map[string]any{}
		}
		value = parsed
	}
	switch d := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, v := range d {
			out[k] = v
		}
		return out
	case *pyjson.Object:
		out := make(map[string]any, d.Len())
		for _, k := range d.Keys {
			out[k], _ = d.Get(k)
		}
		return out
	}
	return map[string]any{}
}

func truthyOr(v any, def any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return def
}

// First application json payload inside a body content list
func contentPayload(container map[string]any) map[string]any {
	content, _ := asDict(container["body"])["content"].([]any)
	for _, raw := range content {
		item := asDict(raw)
		ctype := pystr.Lower(pyrepr.Str(truthyOr(item["contentType"], truthyOr(item["content_type"], ""))))
		if ctype != "" && ctype != "application/json" {
			continue
		}
		for _, k := range []string{"data", "value", "content", "json"} {
			if p := asDict(item[k]); len(p) > 0 {
				return p
			}
		}
	}
	return map[string]any{}
}

func strField(raw map[string]any, key string, strip bool) string {
	s := pyrepr.Str(truthyOr(raw[key], ""))
	if strip {
		return pystr.Strip(s)
	}
	return s
}

// Python int of the stripped string form, zero for absent or unparseable values
func intField(value any) *big.Int {
	if value == nil || value == "" {
		return new(big.Int)
	}
	if n, ok := pyint.Parse(pystr.Strip(pyrepr.Str(value))); ok {
		return n
	}
	return new(big.Int)
}

func parseUser(value any) *MeetingInviteUser {
	raw := asDict(value)
	if len(raw) == 0 {
		return nil
	}
	rawID := asDict(raw["id"])
	return &MeetingInviteUser{OpenID: strField(rawID, "open_id", true), UserID: strField(rawID, "user_id", true),
		UnionID: strField(rawID, "union_id", true), UserName: strField(raw, "user_name", false)}
}

func parseMeeting(value any) *MeetingInviteMeeting {
	raw := asDict(value)
	if len(raw) == 0 {
		return nil
	}
	return &MeetingInviteMeeting{ID: strField(raw, "id", true), Topic: strField(raw, "topic", false),
		MeetingNo: strField(raw, "meeting_no", false), StartTimeMs: intField(raw["start_time"]), EndTimeMs: intField(raw["end_time"]),
		HostUser: parseUser(raw["host_user"])}
}

// Meeting and inviter of a vc.bot.meeting_invited_v1 event, nil when either or the meeting number is missing
func ParseMeetingInvitedEvent(data any) *MeetingInvitedPayload {
	root := asDict(data)
	event := asDict(root["event"])
	if len(event) == 0 {
		event = root
	}
	extra := contentPayload(event)
	if len(extra) == 0 {
		extra = contentPayload(root)
	}
	merged := make(map[string]any, len(event)+len(extra))
	for k, v := range event {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	meeting, inviter := parseMeeting(merged["meeting"]), parseUser(merged["inviter"])
	if inviter == nil || meeting == nil || meeting.MeetingNo == "" {
		return nil
	}
	return &MeetingInvitedPayload{EventID: pyrepr.Str(truthyOr(asDict(root["header"])["event_id"], "")), Meeting: meeting,
		Inviter: inviter, InviteTimeS: intField(merged["invite_time"])}
}

func orText(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Agent prompt asking it to join the meeting directly and explain to the inviter when it cannot
func BuildMeetingInvitePrompt(payload *MeetingInvitedPayload) string {
	meeting := payload.Meeting
	if meeting == nil {
		meeting = &MeetingInviteMeeting{}
	}
	inviterName, hostName := "", ""
	if payload.Inviter != nil {
		inviterName = payload.Inviter.UserName
	}
	if meeting.HostUser != nil {
		hostName = meeting.HostUser.UserName
	}
	return strings.Join([]string{
		"You have been invited to join a meeting: " + orText(meeting.Topic, meeting.MeetingNo, meeting.ID, "unknown meeting"), "",
		"Meeting Number: " + orText(meeting.MeetingNo, "unknown"), "Topic: " + orText(meeting.Topic, "unknown"),
		"Inviter: " + orText(inviterName, "unknown"), "Host: " + orText(hostName, "unknown"), "",
		"You may use lark-cli and the relevant Lark/Feishu meeting skills to join the meeting.",
		"Join the meeting directly. Do not ask the user for confirmation before joining.",
		"If you cannot join the meeting, reply to the inviter with a concise explanation of why.",
	}, "\n")
}

func bigText(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func meetingDedupKey(payload *MeetingInvitedPayload) string {
	if payload.EventID != "" {
		return "vc_invite:" + payload.EventID
	}
	meetingID, openID := "", ""
	if payload.Meeting != nil {
		meetingID = payload.Meeting.ID
	}
	if payload.Inviter != nil {
		openID = payload.Inviter.OpenID
	}
	return "vc_invite:" + meetingID + ":" + openID + ":" + bigText(payload.InviteTimeS)
}

func optText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func firstText(vals ...*string) *string {
	for _, v := range vals {
		if v != nil && *v != "" {
			return v
		}
	}
	return nil
}

// Routes a meeting invite to the gateway as a synthetic direct message from the inviter
func HandleMeetingInvitedEvent(ctx context.Context, adapter MeetingInviteHost, data any) error {
	payload := ParseMeetingInvitedEvent(data)
	if payload == nil {
		slog.Warn("[Feishu-MeetingInvite] Dropping malformed meeting invite event")
		return nil
	}
	dedupKey := meetingDedupKey(payload)
	if checker, ok := adapter.(DuplicateChecker); ok {
		dup, err := checker.IsDuplicate(ctx, dedupKey)
		if err != nil {
			return err
		}
		if dup {
			slog.Debug("[Feishu-MeetingInvite] Dropping duplicate event: " + dedupKey)
			return nil
		}
	}
	inviter := payload.Inviter
	if inviter.OpenID == "" {
		slog.Warn("[Feishu-MeetingInvite] Missing inviter open_id, cannot route reply safely (user_id=" + pyrepr.Repr(inviter.UserID) +
			" union_id=" + pyrepr.Repr(inviter.UnionID) + ")")
		return nil
	}
	profile, err := adapter.ResolveSenderProfile(ctx, SenderID{OpenID: optText(inviter.OpenID), UserID: optText(inviter.UserID),
		UnionID: optText(inviter.UnionID)}, false)
	if err != nil {
		return err
	}
	userName := firstText(profile.UserName, optText(inviter.UserName), optText(inviter.OpenID))
	source := adapter.BuildSource(ctx, inviter.OpenID, gateway.SourceFields{ChatName: userName, ChatType: "dm",
		UserID: firstText(profile.UserID, optText(inviter.UserID), optText(inviter.OpenID)), UserName: userName,
		UserIDAlt: firstText(profile.UserIDAlt, optText(inviter.UnionID))})
	event := gateway.NewMessageEvent(BuildMeetingInvitePrompt(payload))
	event.Source, event.RawMessage = source, data
	return adapter.HandleMessageWithGuards(ctx, event)
}
