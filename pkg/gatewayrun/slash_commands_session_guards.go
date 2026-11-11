package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Chat types whose session key is per user, the unknown blank type included
var dmChatTypes = map[string]bool{"dm": true, "direct": true, "private": true, "": true}

// Optional identity field as a string, empty when unset
func sattr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Alternate user id when set else the user id, empty when neither is
func sourceParticipant(source *gateway.SessionSource) string {
	if alt := sattr(source.UserIDAlt); alt != "" {
		return alt
	}
	return sattr(source.UserID)
}

// Row field as str of its value, empty when the value is falsy
func rowStr(row *pyjson.Object, key string) string {
	if row == nil {
		return ""
	}
	v, _ := row.Get(key)
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Origin of the live gateway entry routing to the session id, nil when none resolves
func (r *GatewayRunner) gatewaySessionOriginForID(ctx context.Context, sessionID string) *gateway.SessionSource {
	if r.sessionStore == nil {
		return nil
	}
	entry := r.sessionStore.LookupBySessionID(ctx, sessionID)
	if entry == nil {
		return nil
	}
	return entry.Origin
}

// Whether both sources are the same Matrix room and thread
func sameMatrixRoom(current, origin *gateway.SessionSource) bool {
	return origin != nil && origin.Platform == gateway.PlatformMatrix && current.Platform == gateway.PlatformMatrix &&
		origin.ChatID == current.ChatID && sattr(current.ThreadID) == sattr(origin.ThreadID)
}

// Whether current shares the origin session key, per participant keys requiring the same participant
func (r *GatewayRunner) sameOriginChat(current, origin *gateway.SessionSource) bool {
	if origin == nil || current == nil {
		return false
	}
	if origin.Platform != current.Platform || origin.ChatID != current.ChatID {
		return false
	}
	if sattr(current.ThreadID) != sattr(origin.ThreadID) {
		return false
	}
	if dmChatTypes[pystr.Lower(current.ChatType)] {
		if current.ChatID != "" {
			return true
		}
		cur, org := sourceParticipant(current), sourceParticipant(origin)
		return cur != "" && cur == org
	}
	if r.isSharedSessionSource(current) {
		return true
	}
	cur, org := sourceParticipant(current), sourceParticipant(origin)
	return cur != "" && org != "" && cur == org
}

// Whether every participant of the source shares one session key under the gateway isolation config
func (r *GatewayRunner) isSharedSessionSource(source *gateway.SessionSource) bool {
	group, thread := true, false
	if r.config != nil {
		group, thread = r.config.GroupSessionsPerUser, r.config.ThreadSessionsPerUser
	}
	return gateway.IsSharedMultiUserSession(source, group, thread)
}

// Whether a persisted session row provably belongs to the source session key, legacy rows failing closed
func (r *GatewayRunner) persistedRowProvesOwner(source *gateway.SessionSource, row *pyjson.Object) bool {
	callerSrc := string(source.Platform)
	var rowSrc any
	if row != nil {
		rowSrc, _ = row.Get("source")
	}
	callerUID := sattr(source.UserID)
	if callerUID == "" {
		return false
	}
	rowThread := rowStr(row, "thread_id")
	if !(pyrepr.Truthy(rowSrc) && callerSrc != "" && pyrepr.Str(rowSrc) == callerSrc && rowThread == sattr(source.ThreadID)) {
		return false
	}
	rowUID := rowStr(row, "user_id")
	rowChat := rowStr(row, "chat_id")
	callerChat := source.ChatID
	callerKeysOnAlt := sattr(source.UserIDAlt) != ""
	if dmChatTypes[pystr.Lower(source.ChatType)] {
		if callerKeysOnAlt && !(rowChat != "" && callerChat != "") {
			return false
		}
		return rowUID != "" && rowUID == callerUID && rowChat == callerChat
	}
	if !(rowChat != "" && callerChat != "" && rowChat == callerChat) {
		return false
	}
	if r.isSharedSessionSource(source) {
		return true
	}
	if callerKeysOnAlt {
		return false
	}
	return rowUID != "" && rowUID == callerUID
}

// Whether the source may resume the target session, the live origin deciding before the persisted row
func (r *GatewayRunner) resumeTargetAllowed(ctx context.Context, source *gateway.SessionSource, targetID string, allowOverride bool) bool {
	if allowOverride && r.resumeCallerIsAdmin(source) {
		return true
	}
	if origin := r.gatewaySessionOriginForID(ctx, targetID); origin != nil {
		return r.sameOriginChat(source, origin)
	}
	db := r.sessionDB(ctx)
	if db == nil {
		return false
	}
	row, err := state.AsyncCall(ctx, db, func(d *state.SessionDB) (*pyjson.Object, error) { return d.GetSession(ctx, targetID) })
	if err != nil {
		return false
	}
	if row == nil {
		row = pyjson.NewObject()
	}
	return r.persistedRowProvesOwner(source, row)
}

// Whether a listing row belongs to the caller origin, Matrix room scoped and admin all bypassing
func (r *GatewayRunner) resumeRowVisible(ctx context.Context, source *gateway.SessionSource, row *pyjson.Object, allowAll bool) bool {
	if allowAll && r.resumeCallerIsAdmin(source) {
		return true
	}
	sid := rowStr(row, "id")
	if source.Platform == gateway.PlatformMatrix {
		return sameMatrixRoom(source, r.gatewaySessionOriginForID(ctx, sid))
	}
	return r.resumeTargetAllowed(ctx, source, sid, false)
}
