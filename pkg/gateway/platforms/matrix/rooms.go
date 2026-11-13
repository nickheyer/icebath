package matrix

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Simple value of a state event, the top level key first and then its content
func stateEventValue(evt map[string]any, key string) string {
	if evt == nil {
		return ""
	}
	for _, obj := range []map[string]any{evt, objectOf(evt["content"])} {
		if v := obj[key]; pyTruthy(v) {
			return strOf(v)
		}
	}
	return ""
}

// Joined or invited member count from the state store, then the joined members API, nil when unknown
func (a *MatrixAdapter) getRoomMemberCount(ctx context.Context, roomID string) *int {
	client := a.currentClient()
	if client == nil {
		return nil
	}
	if client.StateStore != nil {
		if members, err := client.StateStore.GetRoomJoinedOrInvitedMembers(ctx, id.RoomID(roomID)); err == nil {
			n := len(members)
			return &n
		}
	}
	if resp, err := client.JoinedMembers(ctx, id.RoomID(roomID)); err == nil && resp.Joined != nil {
		n := len(resp.Joined)
		return &n
	}
	return nil
}

// Stripped string field of a room state event, nil when missing or unreadable
func (a *MatrixAdapter) getRoomStateValue(ctx context.Context, roomID, eventType, key string) *string {
	client := a.currentClient()
	if client == nil {
		return nil
	}
	var content map[string]any
	if err := client.StateEvent(ctx, id.RoomID(roomID), event.Type{Type: eventType, Class: event.StateEventType}, "", &content); err != nil {
		return nil
	}
	value := pystr.Strip(stateEventValue(content, key))
	if value == "" {
		return nil
	}
	return &value
}

// Drops one cached room identity, or all of them for an empty room id, with the lock held
func (a *MatrixAdapter) invalidateRoomIdentitiesLocked(roomID string) {
	if roomID == "" {
		clear(a.roomIdentities)
		clear(a.roomIdentityCachedAt)
		return
	}
	delete(a.roomIdentities, roomID)
	delete(a.roomIdentityCachedAt, roomID)
}

// Room identity with the joined member count as the primary DM signal, cached for the TTL
func (a *MatrixAdapter) resolveRoomIdentity(ctx context.Context, roomID string, forceRefresh bool) *MatrixRoomIdentity {
	a.mu.Lock()
	cached := a.roomIdentities[roomID]
	ttl := a.roomIdentityTTLSeconds
	cacheFresh := ttl <= 0 || monotonic()-a.roomIdentityCachedAt[roomID] <= ttl
	a.mu.Unlock()
	if cached != nil && cacheFresh && !forceRefresh {
		return cached
	}
	roomName := a.getRoomStateValue(ctx, roomID, "m.room.name", "name")
	roomTopic := a.getRoomStateValue(ctx, roomID, "m.room.topic", "topic")
	canonicalAlias := a.getRoomStateValue(ctx, roomID, "m.room.canonical_alias", "alias")
	memberCount := a.getRoomMemberCount(ctx, roomID)
	hasExplicitName := roomName != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	isDirect := a.dmRooms[roomID]
	isLikelyDM := (memberCount != nil && *memberCount <= 2) || (isDirect && !hasExplicitName)
	var serverName *string
	if strings.Contains(roomID, ":") {
		if s := pystr.Strip(roomID[strings.LastIndex(roomID, ":")+1:]); s != "" {
			serverName = &s
		}
	}
	displayName := roomID
	if roomName != nil {
		displayName = *roomName
	} else if canonicalAlias != nil {
		displayName = *canonicalAlias
	}
	chatType := "room"
	if isLikelyDM {
		chatType = "dm"
	}
	identity := &MatrixRoomIdentity{
		RoomID: roomID, RoomName: roomName, RoomTopic: roomTopic, CanonicalAlias: canonicalAlias, ServerName: serverName,
		JoinedMemberCount: memberCount, IsDirectAccountData: isDirect, DisplayName: displayName,
		HasExplicitName: hasExplicitName, ChatType: chatType,
		Conflict: isDirect && hasExplicitName && (memberCount == nil || *memberCount > 2)}
	if len(a.roomIdentities) >= a.roomIdentityCacheMax {
		oldest, oldestAt, found := "", 0.0, false
		for rid, at := range a.roomIdentityCachedAt {
			if !found || at < oldestAt || (at == oldestAt && rid < oldest) {
				oldest, oldestAt, found = rid, at, true
			}
		}
		if oldest != "" {
			a.invalidateRoomIdentitiesLocked(oldest)
		}
	}
	a.roomIdentities[roomID] = identity
	a.roomIdentityCachedAt[roomID] = monotonic()
	return identity
}

// Whether the room resolves as a DM
func (a *MatrixAdapter) isDMRoom(ctx context.Context, roomID string) bool {
	return a.resolveRoomIdentity(ctx, roomID, false).ChatType == "dm"
}

// The m.direct account data mapping, nil when absent or unreadable
func (a *MatrixAdapter) fetchMDirect(ctx context.Context, logFailure bool) map[string]any {
	client := a.currentClient()
	if client == nil {
		return nil
	}
	var content map[string]any
	if err := client.GetAccountData(ctx, "m.direct", &content); err != nil {
		if logFailure {
			slog.Debug(fmt.Sprintf("Matrix: get_account_data('m.direct') failed: %v", err))
		}
		return nil
	}
	return content
}

// Rebuilds the DM flags of joined rooms from m.direct and drops cached identities
func (a *MatrixAdapter) refreshDMCache(ctx context.Context) {
	if a.currentClient() == nil {
		return
	}
	dmData := a.fetchMDirect(ctx, true)
	if dmData == nil {
		return
	}
	dmRoomIDs := map[string]bool{}
	for _, rooms := range dmData {
		if list, ok := rooms.([]any); ok {
			for _, r := range list {
				if s, ok := r.(string); ok {
					dmRoomIDs[s] = true
				}
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dmRooms = map[string]bool{}
	for rid := range a.joinedRooms {
		a.dmRooms[rid] = dmRoomIDs[rid]
	}
	a.invalidateRoomIdentitiesLocked("")
}

// Appends the room under the inviter in m.direct and marks it as a DM locally
func (a *MatrixAdapter) recordDMRoom(ctx context.Context, roomID, inviter string) {
	client := a.currentClient()
	if client == nil {
		return
	}
	dmData := a.fetchMDirect(ctx, false)
	if dmData == nil {
		dmData = map[string]any{}
	}
	roomsForUser, _ := dmData[inviter].([]any)
	if !slices.Contains(roomsForUser, any(roomID)) {
		roomsForUser = append(roomsForUser, roomID)
		dmData[inviter] = roomsForUser
		if err := client.SetAccountData(ctx, "m.direct", dmData); err != nil {
			slog.Warn(fmt.Sprintf("Matrix: failed to update m.direct: %v", err))
		} else {
			slog.Info(fmt.Sprintf("Matrix: recorded %s as DM room (inviter=%s)", roomID, inviter))
		}
	}
	a.mu.Lock()
	a.dmRooms[roomID] = true
	a.invalidateRoomIdentitiesLocked(roomID)
	a.mu.Unlock()
}

// Display name of a member from the state store, the localpart or the raw id otherwise
func (a *MatrixAdapter) getDisplayName(ctx context.Context, roomID, userID string) string {
	if client := a.currentClient(); client != nil && client.StateStore != nil {
		member, err := client.StateStore.TryGetMember(ctx, id.RoomID(roomID), id.UserID(userID))
		if err == nil && member != nil && member.Displayname != "" {
			return member.Displayname
		}
	}
	if strings.HasPrefix(userID, "@") && strings.Contains(userID, ":") {
		return strings.SplitN(userID[1:], ":", 2)[0]
	}
	return userID
}

// Sends a read receipt off the handler path
func (a *MatrixAdapter) backgroundReadReceipt(ctx context.Context, roomID, eventID string) {
	go a.SendReadReceipt(context.WithoutCancel(ctx), roomID, eventID)
}

// Marks the event as fully read and read in the room
func (a *MatrixAdapter) SendReadReceipt(ctx context.Context, roomID, eventID string) bool {
	client := a.currentClient()
	if client == nil {
		return false
	}
	if err := client.SetReadMarkers(ctx, id.RoomID(roomID), &mautrix.ReqSetReadMarkers{
		FullyRead: id.EventID(eventID), Read: id.EventID(eventID)}); err != nil {
		slog.Debug(fmt.Sprintf("Matrix: read receipt failed: %v", err))
		return false
	}
	slog.Debug(fmt.Sprintf("Matrix: sent read receipt for %s in %s", eventID, roomID))
	return true
}

// Runs one client call when connected, logging the outcome at info or debug and the error at the given level
func (a *MatrixAdapter) clientOp(op func(client *mautrix.Client) error, okMsg, errMsg, level string) bool {
	client := a.currentClient()
	if client == nil {
		return false
	}
	if err := op(client); err != nil {
		msg := fmt.Sprintf(errMsg, err)
		switch level {
		case "debug":
			slog.Debug(msg)
		case "error":
			slog.Error(msg)
		default:
			slog.Warn(msg)
		}
		return false
	}
	if level == "debug" {
		slog.Debug(okMsg)
	} else {
		slog.Info(okMsg)
	}
	return true
}

// Redacts an event in the room with an optional reason
func (a *MatrixAdapter) RedactMessage(ctx context.Context, roomID, eventID, reason string) bool {
	return a.clientOp(func(client *mautrix.Client) error {
		_, err := client.RedactEvent(ctx, id.RoomID(roomID), id.EventID(eventID), mautrix.ReqRedact{Reason: reason})
		return err
	}, fmt.Sprintf("Matrix: redacted %s in %s", eventID, roomID), "Matrix: redact error: %v", "warning")
}

// Creates a room and returns its id, refusing public rooms unless allowed
func (a *MatrixAdapter) CreateRoom(ctx context.Context, name, topic string, invite []string, isDirect bool, preset string) string {
	client := a.currentClient()
	if client == nil {
		return ""
	}
	if preset == "public_chat" && !envTruthy(ctx, "MATRIX_ALLOW_PUBLIC_ROOMS", "") {
		slog.Warn("Matrix: refusing to create public room without MATRIX_ALLOW_PUBLIC_ROOMS=true")
		return ""
	}
	switch preset {
	case "private_chat", "public_chat", "trusted_private_chat":
	default:
		preset = "private_chat"
	}
	invitees := make([]id.UserID, 0, len(invite))
	for _, u := range invite {
		invitees = append(invitees, id.UserID(u))
	}
	resp, err := client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Name: name, Topic: topic, Invite: invitees, IsDirect: isDirect, Preset: preset})
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: create_room error: %v", err))
		return ""
	}
	roomID := string(resp.RoomID)
	a.mu.Lock()
	a.joinedRooms[roomID] = true
	a.mu.Unlock()
	label := name
	if label == "" {
		label = "unnamed"
	}
	slog.Info(fmt.Sprintf("Matrix: created room %s (%s)", roomID, label))
	return roomID
}

// Invites a user to the room
func (a *MatrixAdapter) InviteUser(ctx context.Context, roomID, userID string) bool {
	return a.clientOp(func(client *mautrix.Client) error {
		_, err := client.InviteUser(ctx, id.RoomID(roomID), &mautrix.ReqInviteUser{UserID: id.UserID(userID)})
		return err
	}, fmt.Sprintf("Matrix: invited %s to %s", userID, roomID), "Matrix: invite error: %v", "warning")
}

// Sets presence to online, offline or unavailable with an optional status message
func (a *MatrixAdapter) SetPresence(ctx context.Context, state, statusMsg string) bool {
	if a.currentClient() == nil {
		return false
	}
	presence := map[string]event.Presence{"online": event.PresenceOnline, "offline": event.PresenceOffline, "unavailable": event.PresenceUnavailable}
	p, ok := presence[state]
	if !ok {
		slog.Warn(fmt.Sprintf("Matrix: invalid presence state '%s'", state))
		return false
	}
	return a.clientOp(func(client *mautrix.Client) error {
		return client.SetPresence(ctx, mautrix.ReqPresence{Presence: p, StatusMsg: statusMsg})
	}, fmt.Sprintf("Matrix: presence set to %s", state), "Matrix: set_presence failed: %v", "debug")
}
