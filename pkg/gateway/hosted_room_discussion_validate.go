package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	MaxDiscussionMembers    = 6
	MinDiscussionMembers    = 2
	MaxDiscussionRounds     = 3
	MaxDiscussionMessages   = 10
	maxDiscussionDeltaLines = 24
	maxUserTextBytes        = 64 * 1024
	maxMemberTextBytes      = 64 * 1024
	discussionTruncatedNote = "\n\n[Reply truncated. Ask the Bot to share the full result as a file.]"
	memberControlRelabel    = "[member-quoted "
)

var (
	discussionMentionRE  = regexp.MustCompile(`(?i)@([A-Za-z0-9][A-Za-z0-9._:-]*)`)
	memberControlFrameRE = regexp.MustCompile(`(?i)\[(?:` + strings.Join(mapStrings(state.ControlFrameOpeners, func(opener string) string {
		return strings.ReplaceAll(opener, "]", `\]`)
	}), "|") + `)`)
	discussionTurnIDRE   = regexp.MustCompile(`^d([1-9][0-9]*)\.r(0|[1-9][0-9]*)\.p([0-5])\.s([1-9][0-9]*)\.m([0-9a-f]{24})$`)
	discussionPassRE     = regexp.MustCompile(`(?i)^\(?[\s\x{1c}-\x{1f}\x{85}\pZ]*pass[\s\x{1c}-\x{1f}\x{85}\pZ]*\)?\.?$`)
	discussionDigestRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	discussionTargetKeys = map[string]map[string]bool{"local": {"kind": true, "profile": true},
		"peer": {"kind": true, "peer_id": true, "installation_id": true, "profile": true, "capability_digest": true}}
	discussionRemoteFields = map[string]bool{"connectionId": true, "connectionKind": true, "connectionLabel": true, "connection_id": true, "connection_kind": true,
		"connection_label": true, "remoteSource": true, "route": true, "sourceMissing": true, "sourceReachable": true, "sourceScoped": true, "targetProfile": true, "target_profile": true}
	discussionCoordinateFields = []string{"discussion_event_id", "member_id", "member_index", "round_index", "task_id", "thread_id", "turn_id"}
	discussionTerminalExtras   = map[string][]string{"turn.settled": {"message_event_id", "passed"}, "turn.failed": {"error"},
		"turn.cancelled": {"reason"}, "turn.deferred": {"execution_generation", "reason"}}
	discussionGatewayEvents = map[string]struct {
		fields      []string
		identifiers []string
	}{"room.activity": {[]string{"status", "reason_code", "thread_id", "discussion_event_id"}, []string{"reason_code", "thread_id", "discussion_event_id"}},
		"room.stop_requested": {[]string{"cancel_id"}, []string{"cancel_id"}}}
)

// Set of the named keys
func keySet(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	return out
}

// Invalid policy input or unreconstructable state, a ValueError
type DiscussionPolicyError struct{ Msg string }

func (e *DiscussionPolicyError) Error() string      { return e.Msg }
func (e *DiscussionPolicyError) PyTypeName() string { return "DiscussionPolicyError" }

// Malformed room, roster, payload or typed event
type DiscussionValidationError struct{ *DiscussionPolicyError }

func (e *DiscussionValidationError) Unwrap() error      { return e.DiscussionPolicyError }
func (e *DiscussionValidationError) PyTypeName() string { return "DiscussionValidationError" }

// Persisted task that durable state cannot reproduce
type DiscussionReconstructionError struct{ *DiscussionPolicyError }

func (e *DiscussionReconstructionError) Unwrap() error      { return e.DiscussionPolicyError }
func (e *DiscussionReconstructionError) PyTypeName() string { return "DiscussionReconstructionError" }

func discussionInvalid(msg string) error {
	return &DiscussionValidationError{&DiscussionPolicyError{msg}}
}
func discussionUnreconstructable(msg string) error {
	return &DiscussionReconstructionError{&DiscussionPolicyError{msg}}
}

// Finite per send scheduling ceilings, separate from replay validation
type DiscussionLimits struct {
	MaxRounds   int64
	MaxMessages int64
}

// Validated scheduling ceilings
func NewDiscussionLimits(maxRounds, maxMessages any) (DiscussionLimits, error) {
	rounds, err := discussionPositiveInt(maxRounds, "hosted_rooms.discussion.max_rounds")
	if err != nil {
		return DiscussionLimits{}, err
	}
	messages, err := discussionPositiveInt(maxMessages, "hosted_rooms.discussion.max_messages")
	if err != nil {
		return DiscussionLimits{}, err
	}
	return DiscussionLimits{rounds, messages}, nil
}

// Default scheduling ceilings
func DefaultDiscussionLimits() DiscussionLimits {
	return DiscussionLimits{MaxDiscussionRounds, MaxDiscussionMessages}
}

// One immutable local or peer member of the hosted room
type DiscussionMember struct {
	MemberID    string
	Profile     string
	Handle      string
	DisplayName string
	Target      map[string]any
}

// Whether two members are the same frozen roster entry
func (m DiscussionMember) Equal(other DiscussionMember) bool {
	return m.MemberID == other.MemberID && m.Profile == other.Profile && m.Handle == other.Handle && m.DisplayName == other.DisplayName &&
		pyjson.Equal(m.Target, other.Target) && (m.Target == nil) == (other.Target == nil)
}

// Validated policy projection of one active hosted room
type DiscussionRoom struct {
	RoomID         string
	Name           string
	Members        []DiscussionMember
	GatewayID      string
	AuthorityEpoch int64
}

// Whether the room roster holds member
func (r DiscussionRoom) hasMember(member DiscussionMember) bool {
	for _, candidate := range r.Members {
		if candidate.Equal(member) {
			return true
		}
	}
	return false
}

// One room log event validated against the room
type discussionEvent struct {
	raw     map[string]any
	seq     int64
	eventID string
	kind    string
	actor   map[string]any
	payload map[string]any
}

// Bounded identifier of a discussion field
func discussionIdentifier(value any, label string) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: label, maxChars: maxDriverIdentifierChar}, discussionInvalid)
}

// Exact field check of a discussion mapping
func discussionExactFields(value any, label string, required []string, optional ...string) (map[string]any, error) {
	return roomExactFields(value, label, keySet(required...), keySet(optional...), discussionInvalid, roomFieldFormats{})
}

// Positive integer of a discussion field
func discussionPositiveInt(value any, label string) (int64, error) {
	n, err := roomBoundedInt(value, discussionInvalid, label+" must be a positive integer", 1, nil)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Peer id of a peer member, empty for a local one
func discussionPeerID(member DiscussionMember) (string, bool) {
	if member.Target != nil && member.Target["kind"] == "peer" {
		id, _ := member.Target["peer_id"].(string)
		return id, true
	}
	return "", false
}

// Room log actor of a member, connection id and display name only when present
func discussionMemberActor(member DiscussionMember, displayName bool) map[string]any {
	actor := map[string]any{"kind": "member", "id": member.MemberID, "profile": member.Profile}
	if peerID, ok := discussionPeerID(member); ok {
		actor["connection_id"] = peerID
	}
	if displayName && member.DisplayName != "" {
		actor["display_name"] = member.DisplayName
	}
	return actor
}

// Validated and normalized message.user payload
func ValidateUserPayload(value any) (map[string]any, error) {
	payload, err := discussionExactFields(value, "user payload", []string{"text", "thread_id"}, "attachments")
	if err != nil {
		return nil, err
	}
	threadID, err := discussionIdentifier(payload["thread_id"], "thread_id")
	if err != nil {
		return nil, err
	}
	normalized := map[string]any{"thread_id": threadID}
	if attachments, has := payload["attachments"]; has {
		manifest, err := discussionMessageManifest(attachments)
		if err != nil {
			return nil, err
		}
		normalized["attachments"] = manifest
	}
	text, ok := payload["text"].(string)
	if !ok || len(text) > maxUserTextBytes {
		return nil, discussionInvalid("user payload text must be a bounded string")
	}
	if pystr.Strip(text) == "" && !pyrepr.Truthy(normalized["attachments"]) {
		return nil, discussionInvalid("user payload must contain text or attachments")
	}
	normalized["text"] = pystr.Strip(text)
	return normalized, nil
}

// Validated message attachment manifest under the discussion error
func discussionMessageManifest(value any) ([]any, error) {
	manifest, err := ValidateAttachmentManifest(value)
	if err != nil {
		return nil, discussionInvalid(err.Error())
	}
	return toAnyList(manifest), nil
}

// Validated member target, a local target of a gateway profile or an exact peer route
func discussionMemberTarget(value any, profile string, known map[string]bool, index int) (map[string]any, error) {
	local := func() (map[string]any, error) {
		if !known[profile] {
			return nil, discussionInvalid(fmt.Sprintf("member %d profile '%s' is not local to this gateway", index, profile))
		}
		return map[string]any{"kind": "local", "profile": profile}, nil
	}
	if value == nil {
		return local()
	}
	fields, ok := hostedMapping(value)
	if !ok {
		return nil, discussionInvalid(fmt.Sprintf("member %d target must be an object", index))
	}
	kind, _ := fields["kind"].(string)
	required, known2 := discussionTargetKeys[kind]
	if !known2 {
		return nil, discussionInvalid(fmt.Sprintf("member %d target kind must be local or peer", index))
	}
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	target, err := discussionExactFields(fields, fmt.Sprintf("member %d %s target", index, kind), keys)
	if err != nil {
		return nil, err
	}
	targetProfile, err := discussionIdentifier(target["profile"], fmt.Sprintf("member %d target profile", index))
	if err != nil {
		return nil, err
	}
	if kind == "local" {
		if targetProfile != profile {
			return nil, discussionInvalid(fmt.Sprintf("member %d local target profile does not match member profile", index))
		}
		return local()
	}
	if targetProfile != profile {
		return nil, discussionInvalid(fmt.Sprintf("member %d peer target profile does not match member profile", index))
	}
	digest, ok := target["capability_digest"].(string)
	if !ok || !discussionDigestRE.MatchString(digest) {
		return nil, discussionInvalid(fmt.Sprintf("member %d capability_digest must be a sha256 digest", index))
	}
	peerID, err := discussionIdentifier(target["peer_id"], fmt.Sprintf("member %d peer_id", index))
	if err != nil {
		return nil, err
	}
	installation, err := discussionIdentifier(target["installation_id"], fmt.Sprintf("member %d installation_id", index))
	if err != nil {
		return nil, err
	}
	return map[string]any{"kind": "peer", "peer_id": peerID, "installation_id": installation, "profile": targetProfile, "capability_digest": digest}, nil
}

// Validated roster member at index
func discussionMember(raw any, index int, known map[string]bool) (DiscussionMember, error) {
	fields, ok := hostedMapping(raw)
	if !ok {
		return DiscussionMember{}, discussionInvalid(fmt.Sprintf("member %d must be an object", index))
	}
	var remote []string
	for key := range fields {
		if discussionRemoteFields[key] {
			remote = append(remote, key)
		}
	}
	if len(remote) > 0 {
		sort.Strings(remote)
		return DiscussionMember{}, discussionInvalid(fmt.Sprintf("member %d contains cross-gateway fields: %s", index, strings.Join(remote, ", ")))
	}
	member, err := discussionExactFields(fields, fmt.Sprintf("member %d", index), []string{"member_id", "profile", "handle"}, "display_name", "target")
	if err != nil {
		return DiscussionMember{}, err
	}
	var out DiscussionMember
	for _, item := range []struct {
		field, label string
		dest         *string
	}{{"member_id", "id", &out.MemberID}, {"profile", "profile", &out.Profile}, {"handle", "handle", &out.Handle}} {
		if *item.dest, err = discussionIdentifier(member[item.field], fmt.Sprintf("member %d %s", index, item.label)); err != nil {
			return DiscussionMember{}, err
		}
	}
	if out.Target, err = discussionMemberTarget(member["target"], out.Profile, known, index); err != nil {
		return DiscussionMember{}, err
	}
	displayValue, has := member["display_name"]
	if !has {
		displayValue = ""
	}
	display, ok := displayValue.(string)
	if !ok {
		return DiscussionMember{}, discussionInvalid(fmt.Sprintf("member %d display_name must be a string", index))
	}
	if display = pystr.Strip(display); utf8.RuneCountInString(display) > maxActorLabelChars {
		return DiscussionMember{}, discussionInvalid(fmt.Sprintf("member %d display_name is too long", index))
	}
	out.DisplayName = display
	return out, nil
}

// Validated frozen roster of two to six members on this gateway
func ValidateRoster(value any, localProfiles []string) ([]DiscussionMember, error) {
	list, ok := pyjson.Plain(value).([]any)
	if !ok {
		return nil, discussionInvalid("members must be a list")
	}
	if len(list) < MinDiscussionMembers || len(list) > MaxDiscussionMembers {
		return nil, discussionInvalid(fmt.Sprintf("members must contain between %d and %d entries", MinDiscussionMembers, MaxDiscussionMembers))
	}
	known := map[string]bool{}
	for _, profile := range localProfiles {
		normalized, err := discussionIdentifier(profile, "local profile")
		if err != nil {
			return nil, err
		}
		known[normalized] = true
	}
	targets, handles, ids := map[string]bool{}, map[string]bool{"all": true, "everyone": true}, map[string]bool{}
	members := make([]DiscussionMember, 0, len(list))
	for index, raw := range list {
		member, err := discussionMember(raw, index, known)
		if err != nil {
			return nil, err
		}
		unique := "targets"
		if member.Target["kind"] == "local" {
			unique = "profiles"
		}
		targetKey, err := roomCompactJSON(member.Target, false)
		if err != nil {
			return nil, err
		}
		for _, check := range []struct {
			key     string
			seen    map[string]bool
			message string
		}{{pystr.Casefold(targetKey), targets, "member " + unique + " must be unique"},
			{pystr.Casefold(member.Handle), handles, "member handles must be unique and cannot reserve @all or @everyone"},
			{pystr.Casefold(member.MemberID), ids, "member ids must be unique"}} {
			if check.seen[check.key] {
				return nil, discussionInvalid(check.message)
			}
			check.seen[check.key] = true
		}
		members = append(members, member)
	}
	return members, nil
}

// Strict same gateway policy shape of a hosted room row
func ValidateDiscussionRoom(value any, localProfiles []string) (DiscussionRoom, error) {
	fields, ok := hostedMapping(value)
	if !ok {
		return DiscussionRoom{}, discussionInvalid("room must be an object")
	}
	if fields["disbanded_at"] != nil {
		return DiscussionRoom{}, discussionInvalid("room is disbanded")
	}
	roomID, err := discussionIdentifier(fields["room_id"], "room_id")
	if err != nil {
		return DiscussionRoom{}, err
	}
	name, ok := fields["name"].(string)
	if !ok || pystr.Strip(name) == "" {
		return DiscussionRoom{}, discussionInvalid("room name must be a non-empty string")
	}
	if name = pystr.Strip(name); utf8.RuneCountInString(name) > maxRoomNameChars {
		return DiscussionRoom{}, discussionInvalid("room name is too long")
	}
	gatewayID, err := discussionIdentifier(fields["authority_gateway_id"], "authority_gateway_id")
	if err != nil {
		return DiscussionRoom{}, err
	}
	epoch, err := discussionPositiveInt(fields["authority_epoch"], "authority_epoch")
	if err != nil {
		return DiscussionRoom{}, err
	}
	members, err := ValidateRoster(fields["members"], localProfiles)
	if err != nil {
		return DiscussionRoom{}, err
	}
	return DiscussionRoom{roomID, name, members, gatewayID, epoch}, nil
}

// Whether a settled member result is Discussion silence
func IsPassText(value any) bool {
	text := pystr.Strip(roomLinkText(value))
	return text == "" || discussionPassRE.MatchString(text)
}

// Members the texts mention by handle, everyone for @all or for no mention when defaultAll
func ResolveMentions(texts []string, members []DiscussionMember, defaultAll bool) []DiscussionMember {
	byHandle := map[string]bool{}
	for _, member := range members {
		byHandle[pystr.Casefold(member.Handle)] = true
	}
	mentioned, everyone := map[string]bool{}, false
	for _, text := range texts {
		for _, match := range discussionMentionRE.FindAllStringSubmatch(text, -1) {
			handle := pystr.Casefold(match[1])
			if handle == "all" || handle == "everyone" {
				everyone = true
			} else if byHandle[handle] {
				mentioned[handle] = true
			}
		}
	}
	if everyone || (defaultAll && len(mentioned) == 0) {
		return append([]DiscussionMember{}, members...)
	}
	var out []DiscussionMember
	for _, member := range members {
		if mentioned[pystr.Casefold(member.Handle)] {
			out = append(out, member)
		}
	}
	return out
}

// Peers a Bot explicitly cited and not heard from afterward
func discussionUnaddressedMentions(messages []discussionEvent, room DiscussionRoom) []DiscussionMember {
	citedAt, lastPostAt := map[string]int64{}, map[string]int64{}
	for _, event := range messages {
		if event.kind != "message.member" {
			continue
		}
		speaker := pyrepr.Str(event.payload["member_id"])
		lastPostAt[speaker] = event.seq
		for _, member := range ResolveMentions([]string{pyrepr.Str(event.payload["text"])}, room.Members, false) {
			if member.MemberID != speaker {
				citedAt[member.MemberID] = event.seq
			}
		}
	}
	var out []DiscussionMember
	for _, member := range room.Members {
		if cited, has := citedAt[member.MemberID]; has && lastPostAt[member.MemberID] <= cited {
			out = append(out, member)
		}
	}
	return out
}

// Refuses an actor other than the room gateway
func discussionRequireGateway(actor map[string]any, room DiscussionRoom, message string) error {
	if actor["kind"] != "gateway" || actor["id"] != room.GatewayID {
		return discussionInvalid(message)
	}
	return nil
}

// Roster member of an id
func discussionMemberByID(room DiscussionRoom, memberID any) (DiscussionMember, error) {
	normalized, err := discussionIdentifier(memberID, "member_id")
	if err != nil {
		return DiscussionMember{}, err
	}
	for _, member := range room.Members {
		if member.MemberID == normalized {
			return member, nil
		}
	}
	return DiscussionMember{}, discussionInvalid(fmt.Sprintf("unknown Discussion member '%s'", normalized))
}

// Validates the coordinate fields every member message and terminal event carries
func discussionTurnCoordinates(payload map[string]any, room DiscussionRoom) error {
	if _, err := discussionMemberByID(room, payload["member_id"]); err != nil {
		return err
	}
	if _, err := roomBoundedInt(payload["member_index"], discussionInvalid, fmt.Sprintf("member_index must be an integer between 0 and %d", MaxDiscussionMembers-1), 0, big.NewInt(MaxDiscussionMembers-1)); err != nil {
		return err
	}
	if _, err := roomBoundedInt(payload["round_index"], discussionInvalid, "round_index must be a non-negative integer", 0, nil); err != nil {
		return err
	}
	for _, field := range []string{"thread_id", "task_id", "turn_id", "discussion_event_id"} {
		if _, err := discussionIdentifier(payload[field], field); err != nil {
			return err
		}
	}
	return nil
}

// Validated payload of one typed room event, unknown kinds passing through
func discussionValidatePayload(kind string, payload, actor map[string]any, room DiscussionRoom) (map[string]any, error) {
	switch {
	case kind == "message.user":
		normalized, err := ValidateUserPayload(payload)
		if err != nil {
			return nil, err
		}
		if actor["kind"] != "user" {
			return nil, discussionInvalid("message.user requires a user actor")
		}
		return normalized, nil
	case kind == "message.member":
		return discussionValidateMemberMessage(payload, actor, room)
	case discussionTerminalExtras[kind] != nil:
		return discussionValidateTerminal(kind, payload, actor, room)
	}
	spec := discussionGatewayEvents[kind]
	if _, err := discussionExactFields(payload, kind+" payload", spec.fields); err != nil {
		return nil, err
	}
	if status := payload["status"]; kind == "room.activity" && status != "settled" && status != "bounded" {
		return nil, discussionInvalid("invalid room.activity status")
	}
	for _, field := range spec.identifiers {
		if _, err := discussionIdentifier(payload[field], field); err != nil {
			return nil, err
		}
	}
	return payload, discussionRequireGateway(actor, room, kind+" requires the room gateway")
}

// Validated member message payload whose actor matches the roster
func discussionValidateMemberMessage(payload, actor map[string]any, room DiscussionRoom) (map[string]any, error) {
	if _, err := discussionExactFields(payload, "message.member payload", append(append([]string{}, discussionCoordinateFields...), "text"), "attachments", "recipient_member_ids"); err != nil {
		return nil, err
	}
	out := pyjson.Plain(payload).(map[string]any)
	if attachments, has := out["attachments"]; has {
		manifest, err := discussionMessageManifest(attachments)
		if err != nil {
			return nil, err
		}
		out["attachments"] = manifest
	}
	if recipients, has := out["recipient_member_ids"]; has {
		list, ok := recipients.([]any)
		if !ok || len(list) == 0 {
			return nil, discussionInvalid("recipient_member_ids must be a non-empty list")
		}
		seen, normalized := map[string]bool{}, make([]any, 0, len(list))
		for _, recipient := range list {
			member, err := discussionMemberByID(room, recipient)
			if err != nil {
				return nil, err
			}
			seen[member.MemberID] = true
			normalized = append(normalized, member.MemberID)
		}
		if len(seen) != len(normalized) {
			return nil, discussionInvalid("recipient_member_ids must be unique")
		}
		out["recipient_member_ids"] = normalized
	}
	if err := discussionTurnCoordinates(out, room); err != nil {
		return nil, err
	}
	if text, ok := out["text"].(string); !ok || pystr.Strip(text) == "" || IsPassText(text) {
		return nil, discussionInvalid("message.member text must be a non-pass string")
	}
	member, err := discussionMemberByID(room, out["member_id"])
	if err != nil {
		return nil, err
	}
	expected := discussionMemberActor(member, false)
	peerID, isPeer := discussionPeerID(member)
	if isPeer {
		expected["connection_id"] = peerID
	} else {
		expected["connection_id"] = nil
	}
	for key, value := range expected {
		if !pyjson.Equal(actor[key], value) {
			return nil, discussionInvalid("message.member actor does not match roster")
		}
	}
	return out, nil
}

// Validated terminal event payload from the room gateway
func discussionValidateTerminal(kind string, payload, actor map[string]any, room DiscussionRoom) (map[string]any, error) {
	required := append(append(append([]string{}, discussionCoordinateFields...), "seen_through_seq"), discussionTerminalExtras[kind]...)
	var optional []string
	if kind == "turn.failed" {
		optional = []string{"reason_code"}
	}
	if _, err := discussionExactFields(payload, kind+" payload", required, optional...); err != nil {
		return nil, err
	}
	if err := discussionTurnCoordinates(payload, room); err != nil {
		return nil, err
	}
	if _, err := discussionPositiveInt(payload["seen_through_seq"], "seen_through_seq"); err != nil {
		return nil, err
	}
	if actor["connection_id"] != nil {
		return nil, discussionInvalid(kind + " requires a gateway actor")
	}
	if err := discussionRequireGateway(actor, room, kind+" requires a gateway actor"); err != nil {
		return nil, err
	}
	if kind == "turn.settled" {
		passed, ok := payload["passed"].(bool)
		if !ok {
			return nil, discussionInvalid("turn.settled passed must be a boolean")
		}
		if !passed {
			if _, err := discussionIdentifier(payload["message_event_id"], "message_event_id"); err != nil {
				return nil, err
			}
		} else if payload["message_event_id"] != nil {
			return nil, discussionInvalid("a passed turn cannot reference a member message")
		}
		return payload, nil
	}
	field := "reason"
	if kind == "turn.failed" {
		field = "error"
	}
	if text, ok := payload[field].(string); !ok || pystr.Strip(text) == "" {
		return nil, discussionInvalid(kind + " " + field + " must be non-empty")
	}
	if kind == "turn.deferred" {
		if _, err := discussionPositiveInt(payload["execution_generation"], "execution_generation"); err != nil {
			return nil, err
		}
	}
	if reason, has := payload["reason_code"]; kind == "turn.failed" && has {
		code, _ := reason.(string)
		if !tools.AllReasons[code] {
			return nil, discussionInvalid("turn.failed reason_code must use the shared failure vocabulary")
		}
	}
	return payload, nil
}

// Validated room event after previousSeq
func discussionValidateEvent(raw any, room DiscussionRoom, previousSeq int64) (discussionEvent, error) {
	fields, ok := hostedMapping(raw)
	if !ok {
		return discussionEvent{}, discussionInvalid("room event must be an object")
	}
	fields = pyjson.Plain(fields).(map[string]any)
	if fields["room_id"] != room.RoomID {
		return discussionEvent{}, discussionInvalid("room event belongs to a different room")
	}
	seq, err := discussionPositiveInt(fields["seq"], "event seq")
	if err != nil {
		return discussionEvent{}, err
	}
	if seq <= previousSeq {
		return discussionEvent{}, discussionInvalid("room events must be in strict sequence order")
	}
	eventID, err := discussionIdentifier(fields["event_id"], "event_id")
	if err != nil {
		return discussionEvent{}, err
	}
	kind, ok := fields["kind"].(string)
	if !ok {
		return discussionEvent{}, discussionInvalid("event kind must be a string")
	}
	actor, ok := fields["actor"].(map[string]any)
	if !ok {
		return discussionEvent{}, discussionInvalid("event actor must be an object")
	}
	payload, ok := fields["payload"].(map[string]any)
	if !ok {
		return discussionEvent{}, discussionInvalid("event payload must be an object")
	}
	_, gatewayKind := discussionGatewayEvents[kind]
	if stamped := discussionTerminalExtras[kind] != nil || kind == "message.member" || gatewayKind; stamped && !pyjson.Equal(fields["authority_epoch"], room.AuthorityEpoch) {
		return discussionEvent{}, discussionInvalid(kind + " authority epoch does not match the room")
	}
	if kind == "message.user" || kind == "message.member" || discussionTerminalExtras[kind] != nil || gatewayKind {
		if payload, err = discussionValidatePayload(kind, payload, actor, room); err != nil {
			return discussionEvent{}, err
		}
	}
	return discussionEvent{raw: fields, seq: seq, eventID: eventID, kind: kind, actor: actor, payload: payload}, nil
}

// Every event validated in strict order with unique ids
func discussionValidatedEvents(events []any, room DiscussionRoom) ([]discussionEvent, error) {
	validated := make([]discussionEvent, 0, len(events))
	ids := map[string]bool{}
	for _, raw := range events {
		var previous int64
		if len(validated) > 0 {
			previous = validated[len(validated)-1].seq
		}
		event, err := discussionValidateEvent(raw, room, previous)
		if err != nil {
			return nil, err
		}
		if ids[event.eventID] {
			return nil, discussionInvalid("room event ids must be unique")
		}
		validated = append(validated, event)
		ids[event.eventID] = true
	}
	return validated, nil
}

// Watermarks of each thread and member derived from terminal events of a room log
func DeriveMemberWatermarks(roomValue any, events []any, localProfiles []string) (map[[2]string]int64, error) {
	room, err := ValidateDiscussionRoom(roomValue, localProfiles)
	if err != nil {
		return nil, err
	}
	validated, err := discussionValidatedEvents(events, room)
	if err != nil {
		return nil, err
	}
	return discussionDeriveWatermarks(validated)
}

// Watermarks of validated events, refusing duplicate terminals and unmatched settled messages
func discussionDeriveWatermarks(events []discussionEvent) (map[[2]string]int64, error) {
	messages := map[string]discussionEvent{}
	for _, event := range events {
		if event.kind == "message.member" {
			messages[event.eventID] = event
		}
	}
	terminalByTask := map[string]discussionEvent{}
	watermarks := map[[2]string]int64{}
	for _, event := range events {
		if discussionTerminalExtras[event.kind] == nil {
			continue
		}
		taskID := pyrepr.Str(event.payload["task_id"])
		if previous, has := terminalByTask[taskID]; has {
			if previous.kind != "turn.deferred" {
				return nil, discussionInvalid(fmt.Sprintf("task '%s' has more than one terminal room event", taskID))
			}
			current, _ := hostedStrictInt(event.payload["execution_generation"])
			prior, _ := hostedStrictInt(previous.payload["execution_generation"])
			if event.kind == "turn.deferred" && current <= prior {
				return nil, discussionInvalid(fmt.Sprintf("task '%s' deferral generation did not advance", taskID))
			}
		}
		terminalByTask[taskID] = event
		key := [2]string{pyrepr.Str(event.payload["thread_id"]), pyrepr.Str(event.payload["member_id"])}
		watermark, _ := hostedStrictInt(event.payload["seen_through_seq"])
		if passed, _ := event.payload["passed"].(bool); event.kind == "turn.settled" && !passed {
			message, has := messages[pyrepr.Str(event.payload["message_event_id"])]
			if !has || !pyjson.Equal(message.payload["task_id"], event.payload["task_id"]) || !pyjson.Equal(message.payload["member_id"], event.payload["member_id"]) ||
				!pyjson.Equal(message.payload["thread_id"], event.payload["thread_id"]) {
				return nil, discussionInvalid("turn.settled references no matching member message")
			}
			discussionSeq := int64(-1)
			for _, candidate := range events {
				if candidate.eventID == event.payload["discussion_event_id"] {
					discussionSeq = candidate.seq
					break
				}
			}
			if discussionSeq < 0 {
				return nil, discussionInvalid("turn.settled references no matching user discussion")
			}
			if watermark >= discussionSeq {
				watermark = max(watermark, message.seq)
			}
		}
		watermarks[key] = max(watermarks[key], watermark)
	}
	return watermarks, nil
}

// Stable 24 hex digest of a member's identity and target
func discussionMemberDigest(member DiscussionMember) string {
	target := member.Target
	if target == nil {
		target = map[string]any{"kind": "local", "profile": member.Profile}
	}
	encoded, _ := roomCompactJSON(target, false)
	sum := sha256.Sum256([]byte(member.MemberID + "\x00" + member.Profile + "\x00" + member.Handle + "\x00" + encoded))
	return hex.EncodeToString(sum[:])[:24]
}
