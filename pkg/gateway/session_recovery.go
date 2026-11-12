package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Routing peer recorder of hermes_state_gateway record_gateway_session_peer
type gatewaySessionPeerRecorder interface {
	RecordGatewaySessionPeer(ctx context.Context, sessionID string, p state.GatewayPeer) error
}

// Peer row finder of hermes_state_gateway find_latest_gateway_session_for_peer
type gatewaySessionPeerFinder interface {
	FindLatestGatewaySessionForPeer(ctx context.Context, q state.GatewayPeerQuery) (*pyjson.Object, error)
}

// Reset promotion that upgrades an accidental end reason to an explicit boundary, true when a row was written
type sessionResetPromoter interface {
	PromoteToSessionReset(ctx context.Context, sessionID, reason string) bool
}

// Python repr of a decoded value
func pyReprValue(v any) string { return pyrepr.Repr(v) }

// Python repr of a string
func pyReprString(v string) string { return pyrepr.Repr(v) }

// Source dict as JSON, empty when absent or unserializable
func originJSON(source *SessionSource) string {
	if source == nil {
		return ""
	}
	text, err := pyjson.Dumps(source.ToDict())
	if err != nil {
		return ""
	}
	return text
}

// Profile namespace for session keys, nil when multiplexing is off
func (s *SessionStore) ResolveProfileForKey(source *SessionSource) (any, error) {
	if !s.multiplexProfiles() {
		return nil, nil
	}
	if identity := IdentityOf(source); identity != nil {
		return identity.SessionKeyProfile(), nil
	}
	if source != nil && source.Profile != nil && *source.Profile != "" {
		return *source.Profile, nil
	}
	if name := profiles.GetActiveProfileName(s.ctx); name != "" {
		return name, nil
	}
	return "default", nil
}

// Profile namespace encoded in a gateway session key
func profileFromSessionKey(sessionKey string) (string, bool) {
	return state.ProfileFromSessionKey(sessionKey)
}

// Active profile name, default when unset
func (s *SessionStore) activeProfileName() string {
	if name := profiles.GetActiveProfileName(s.ctx); name != "" {
		return name
	}
	return "default"
}

// Whether a recovered row may be revived by this gateway's profile, keyless rows staying adoptable
func (s *SessionStore) recoveredRowAllowedForActiveProfile(requestedSessionKey string, recovered *pyjson.Object) bool {
	raw, _ := recovered.Get("session_key")
	recoveredKey := ""
	if pyTruthy(raw) {
		recoveredKey = pyStrOf(raw)
	}
	if recoveredKey == "" || recoveredKey == requestedSessionKey {
		return true
	}
	recoveredProfile, ok := profileFromSessionKey(recoveredKey)
	if !ok {
		return true
	}
	if s.multiplexProfiles() {
		requested, ok := profileFromSessionKey(requestedSessionKey)
		return !ok || recoveredProfile == requested
	}
	return recoveredProfile == s.activeProfileName()
}

// Session key for the source, the key built from keySource when given
func (s *SessionStore) generateSessionKey(ctx context.Context, source, keySource *SessionSource) (string, error) {
	group, thread := true, false
	if s.Config != nil {
		group, thread = s.Config.GroupSessionsPerUser, s.Config.ThreadSessionsPerUser
	}
	profile, err := s.ResolveProfileForKey(source)
	if err != nil {
		return "", err
	}
	name, _ := profile.(string)
	if keySource == nil {
		keySource = source
	}
	return BuildSessionKey(ctx, keySource, group, thread, name)
}

// Pre workspace Slack key for an explicitly scoped source, empty otherwise
func (s *SessionStore) legacySlackSessionKey(ctx context.Context, source *SessionSource) string {
	if source.Platform != PlatformSlack || source.ScopeID == nil || *source.ScopeID == "" {
		return ""
	}
	unscoped := ReplaceSource(source, func(src *SessionSource) { src.ScopeID, src.GuildID = nil, nil })
	key, err := s.generateSessionKey(ctx, source, unscoped)
	if err != nil {
		return ""
	}
	return key
}

// Reserves one ambiguous legacy Slack key for migration once per process
func (s *SessionStore) claimLegacySlackKey(legacyKey string) bool {
	if legacyKey == "" {
		return false
	}
	s.legacySlackClaimMu.Lock()
	defer s.legacySlackClaimMu.Unlock()
	if s.claimedLegacySlackKeys[legacyKey] {
		return false
	}
	s.claimedLegacySlackKeys[legacyKey] = true
	return true
}

// Rejects a recovered row whose origin belongs to another Slack workspace
func recoveredRowMatchesSourceScope(recovered *pyjson.Object, source *SessionSource) bool {
	if source.Platform != PlatformSlack || source.ChatType == "dm" || source.ScopeID == nil || *source.ScopeID == "" {
		return true
	}
	raw, _ := recovered.Get("origin_json")
	text, _ := raw.(string)
	if !pyTruthy(raw) {
		text = ""
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return false
	}
	origin, ok := parsed.(*pyjson.Object)
	if !ok {
		return false
	}
	scope, ok := origin.Get("scope_id")
	if !ok {
		scope, _ = origin.Get("guild_id")
	}
	return scope == *source.ScopeID
}

// Epoch value of a row column as a local naive datetime, the default when invalid
func rowTimestamp(value any, def pyDatetime) pyDatetime {
	var f float64
	switch x := value.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	case bool:
		if x {
			f = 1
		}
	case string:
		parsed, err := pyint.ParseFloat(x)
		if err != nil {
			return def
		}
		f = parsed
	default:
		return def
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return def
	}
	sec, frac := math.Modf(f)
	return pyDatetime{T: time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).In(time.Local)}
}

// Entry rebuilt from a recovered row, an invalid durable timestamp reading as old
func (s *SessionStore) createEntryFromRecoveredRow(row *pyjson.Object, sessionKey string, source *SessionSource) *SessionEntry {
	started, _ := row.Get("started_at")
	created := rowTimestamp(started, pyDatetime{T: time.Unix(0, 0).In(time.Local)})
	lastActivity, hasLast := row.Get("last_activity_at")
	updated := created
	if hasLast && lastActivity != nil {
		updated = rowTimestamp(lastActivity, created)
	}
	hadRaw, _ := row.Get("_has_messages")
	had := pyTruthy(hadRaw)
	if hadRaw == nil {
		count, _ := row.Get("message_count")
		had = pyTruthy(count) || (hasLast && lastActivity != nil)
	}
	id, _ := row.Get("id")
	e := NewSessionEntry(sessionKey, pyStrOf(id), created, updated)
	e.Origin, e.DisplayName, e.Platform, e.ChatType = source, source.ChatName, source.Platform, source.ChatType
	e.ResetHadActivity, e.TransportProfile = had, optionalString(TransportProfileOf(source))
	return e
}

// One durable gateway row for the key, the peer tuple fallback only when allowed
func (s *SessionStore) findGatewaySessionRow(ctx context.Context, sessionKey string, source *SessionSource, allowPeerFallback, raiseOnLookupError bool) (*pyjson.Object, error) {
	q := state.GatewayPeerQuery{UserID: source.UserID, ThreadID: source.ThreadID}
	if allowPeerFallback {
		chatID, chatType := source.ChatID, source.ChatType
		q.ChatID, q.ChatType = &chatID, &chatType
	}
	return peerRow(ctx, s.dbForKey(ctx, sessionKey), string(source.Platform), sessionKey, raiseOnLookupError, q)
}

// Guarded peer finder call, nil for a missing store or finder and for a failed lookup unless raising
func peerRow(ctx context.Context, db SessionStoreDB, source, sessionKey string, raiseOnLookupError bool, q state.GatewayPeerQuery) (*pyjson.Object, error) {
	finder, ok := db.(gatewaySessionPeerFinder)
	if db == nil || !ok || finder == nil {
		return nil, nil
	}
	q.Source, q.SessionKey = source, &sessionKey
	row, err := finder.FindLatestGatewaySessionForPeer(ctx, q)
	if err != nil {
		slog.Debug(fmt.Sprintf("Gateway session DB recovery failed for %s: %v", sessionKey, err))
		if raiseOnLookupError {
			return nil, err
		}
		return nil, nil
	}
	return row, nil
}

// Session id and owning store for a key during shutdown flush recovery, never minting a session
func (s *SessionStore) ResolveSessionIDForKey(ctx context.Context, sessionKey string, notAfter *float64) (string, SessionStoreDB) {
	if sessionKey == "" {
		return "", nil
	}
	db := s.dbForKey(ctx, sessionKey)
	if db == nil {
		return "", nil
	}
	if sid, ok := s.PeekSessionID(sessionKey); ok && sid != "" {
		return sid, db
	}
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 || parts[0] != "agent" || parts[2] == "" {
		return "", nil
	}
	row, _ := peerRow(ctx, db, parts[2], sessionKey, false, state.GatewayPeerQuery{})
	if row == nil {
		return "", nil
	}
	id, _ := row.Get("id")
	if !pyTruthy(id) {
		return "", nil
	}
	if started, ok := row.Get("started_at"); ok && started != nil && notAfter != nil {
		f, ok := jsonFloat(started)
		if ok && math.Floor(f) > float64(int64(*notAfter)) {
			return "", nil
		}
	}
	return pyStrOf(id), db
}

// Float value of a decoded JSON number or numeric string
func jsonFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		f, err := pyint.ParseFloat(x)
		return f, err == nil
	}
	return 0, false
}

// Rebuilds a missing key mapping from a recoverable durable row and reopens it
func (s *SessionStore) recoverSessionFromDB(ctx context.Context, sessionKey string, source *SessionSource, now pyDatetime, raiseOnLookupError bool) (*SessionEntry, error) {
	entry, migrated, err := s.queryRecoverableRow(ctx, sessionKey, source, raiseOnLookupError)
	if err != nil || entry == nil {
		return nil, err
	}
	s.reopenSessionRow(ctx, sessionKey, entry.SessionID, "")
	if migrated {
		if err := s.recordGatewaySessionPeer(ctx, entry.SessionID, sessionKey, source, entry.DisplayName, false, nil); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// Database half of recovery without reopening the row, the caller publishing the entry
func (s *SessionStore) queryRecoverableSession(ctx context.Context, sessionKey string, source *SessionSource, now pyDatetime) (*SessionEntry, error) {
	entry, migrated, err := s.queryRecoverableRow(ctx, sessionKey, source, false)
	if err != nil {
		return nil, err
	}
	if entry != nil && migrated {
		if err := s.recordGatewaySessionPeer(ctx, entry.SessionID, sessionKey, source, entry.DisplayName, false, nil); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// Finds and gates a recoverable row including the once per process legacy Slack key lookup
func (s *SessionStore) queryRecoverableRow(ctx context.Context, sessionKey string, source *SessionSource, raiseOnLookupError bool) (*SessionEntry, bool, error) {
	legacyKey := s.legacySlackSessionKey(ctx, source)
	recovered, err := s.findGatewaySessionRow(ctx, sessionKey, source, legacyKey == "", raiseOnLookupError)
	if err != nil {
		return nil, false, err
	}
	migrated := false
	if (recovered == nil || recovered.Len() == 0) && legacyKey != "" && s.claimLegacySlackKey(legacyKey) {
		if recovered, err = s.findGatewaySessionRow(ctx, legacyKey, source, false, raiseOnLookupError); err != nil {
			return nil, false, err
		}
		migrated = recovered != nil && recovered.Len() > 0
	}
	if recovered == nil {
		return nil, false, nil
	}
	if !recoveredRowMatchesSourceScope(recovered, source) {
		return nil, false, nil
	}
	if !s.recoveredRowAllowedForActiveProfile(sessionKey, recovered) {
		key, _ := recovered.Get("session_key")
		slog.Warn(fmt.Sprintf("Gateway session DB recovery ignored %v for %s because the row belongs to a different profile", key, sessionKey))
		return nil, false, nil
	}
	return s.createEntryFromRecoveredRow(recovered, sessionKey, source), migrated, nil
}

// Ends the session with the reason as an explicit reset boundary, reporting failures through log
func (s *SessionStore) promoteSessionReset(ctx context.Context, sessionKey, sessionID, reason string, log func(error)) {
	err := func() error {
		db := s.dbForKey(ctx, sessionKey)
		if db == nil {
			return fmt.Errorf("'NoneType' object has no attribute 'end_session'")
		}
		if promoter, ok := db.(sessionResetPromoter); ok {
			promoter.PromoteToSessionReset(ctx, sessionID, reason)
		} else if err := db.EndSession(sessionID, reason); err != nil {
			return err
		}
		if err := releaseSupersededBotChat(ctx, db, s, sessionKey, sessionID); err != nil {
			return err
		}
		heartbeatKey := "heartbeat:" + sessionID
		value, ok, err := db.GetMeta(ctx, heartbeatKey)
		if err != nil {
			return err
		}
		if ok && value != "" {
			return db.SetMeta(ctx, heartbeatKey, "")
		}
		return nil
	}()
	if err != nil {
		log(err)
	}
}

// Reopens the row, failures logged at debug only
func (s *SessionStore) reopenSessionRow(ctx context.Context, sessionKey, sessionID, logPrefix string) {
	db := s.dbForKey(ctx, sessionKey)
	var err error
	if db == nil {
		err = fmt.Errorf("'NoneType' object has no attribute 'reopen_session'")
	} else {
		err = db.ReopenSession(ctx, sessionID)
	}
	if err == nil {
		return
	}
	if logPrefix != "" {
		slog.Debug(fmt.Sprintf("%s: %v", logPrefix, err))
	} else {
		slog.Debug(fmt.Sprintf("Gateway session DB reopen failed for %s: %v", sessionKey, err))
	}
}

// Persists the routing peer for an existing gateway session row, a storage source refusal returned
func (s *SessionStore) recordGatewaySessionPeer(ctx context.Context, sessionID, sessionKey string, source *SessionSource, displayName *string, includeCompressionAncestors bool, transportProfile *string) error {
	db := s.dbForKey(ctx, sessionKey)
	if db == nil || source == nil {
		return nil
	}
	recorder, ok := db.(gatewaySessionPeerRecorder)
	if !ok || recorder == nil {
		return nil
	}
	fallback, err := apiStorageSource(ctx, db, sessionID, string(source.Platform))
	if err != nil {
		return err
	}
	storage, err := a2aStorageSource(ctx, db, source, sessionID, fallback)
	if err != nil {
		return err
	}
	name := displayName
	if name == nil || *name == "" {
		name = source.ChatName
	}
	transport := ""
	if transportProfile != nil {
		transport = *transportProfile
	}
	if transport == "" {
		transport = TransportProfileOf(source)
	}
	chatID, chatType := source.ChatID, source.ChatType
	peer := state.GatewayPeer{
		Source: storage,
		UserID: source.UserID, SessionKey: &sessionKey, ChatID: &chatID, ChatType: &chatType, ThreadID: source.ThreadID,
		DisplayName: name, OriginJSON: optionalString(originJSON(source)),
		IncludeCompressionAncestors: includeCompressionAncestors, TransportProfile: optionalString(transport),
	}
	if err := recorder.RecordGatewaySessionPeer(ctx, sessionID, peer); err != nil {
		slog.Debug(fmt.Sprintf("Gateway session peer record failed for %s: %v", sessionKey, err))
	}
	return nil
}

// Moves a pre workspace Slack entry onto the scoped key once, refusing ambiguous channels
func (s *SessionStore) adoptLegacySlackEntry(ctx context.Context, source *SessionSource, sessionKey string) error {
	legacyKey := s.legacySlackSessionKey(ctx, source)
	if legacyKey == "" {
		return nil
	}
	var migrated *SessionEntry
	s.mu.Lock()
	s.ensureLoadedLocked(ctx)
	legacy := s.entries.get(legacyKey)
	if !s.entries.has(sessionKey) && legacy != nil {
		adopt := source.ChatType == "dm"
		if legacy.Origin != nil && legacy.Origin.ScopeID != nil {
			adopt = source.ScopeID != nil && *legacy.Origin.ScopeID == *source.ScopeID
		}
		if adopt && s.claimLegacySlackKey(legacyKey) {
			migrated = s.entries.delete(legacyKey)
			migrated.SessionKey, migrated.Origin, migrated.Platform, migrated.ChatType = sessionKey, source, source.Platform, source.ChatType
			s.entries.set(sessionKey, migrated)
		}
	}
	s.mu.Unlock()
	if migrated == nil {
		return nil
	}
	if err := s.saveEntries(ctx); err != nil {
		return err
	}
	return s.recordGatewaySessionPeer(ctx, migrated.SessionID, sessionKey, source, migrated.DisplayName, false, nil)
}

// Database side of a routing transition outside the lock, promoting the predecessor then creating the row
func (s *SessionStore) finishRouteTransition(ctx context.Context, sessionKey string, endSessionID *string, endReason string, create *sessionCreateArgs, origin *SessionSource, displayName *string, during string) {
	if endSessionID != nil && *endSessionID != "" {
		observability.CloseSessionRun(ctx, *endSessionID)
	}
	if s.dbForKey(ctx, sessionKey) != nil && endSessionID != nil && *endSessionID != "" {
		endID := *endSessionID
		s.promoteSessionReset(ctx, sessionKey, endID, endReason, func(err error) {
			slog.Warn(fmt.Sprintf("Failed to end predecessor session row %s for %s%s: %v — the old row remains "+
				"open and may win restart recovery until the next successful peer refresh", endID, sessionKey, during, err))
		})
	}
	if s.dbForKey(ctx, sessionKey) != nil && create != nil {
		s.createSessionRow(ctx, sessionKey, create, origin, displayName, func(err error) {
			slog.Warn(fmt.Sprintf("Failed to create session row %s for %s%s: %v — deferring to the "+
				"self-healing peer refresh on the next turn", create.SessionID, sessionKey, during, err))
		})
	}
}

// Tells the owner the key now names the entry's conversation
func (s *SessionStore) announceRoute(sessionKey string, entry *SessionEntry) {
	if s.RouteNamed != nil && entry != nil {
		s.RouteNamed(sessionKey, entry.SessionID, entry.Origin)
	}
}

// Marks the published id's row transition finished so a missing row is real again
func (s *SessionStore) rowSettled(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rowsPending, sessionID)
}

// Create session arguments landing identity and lineage atomically in the insert
type sessionCreateArgs struct {
	SessionID string
	Row       state.NewSession
}

// Create session arguments for the routed session with its origin and parent lineage
func sessionCreateKwargs(sessionID, sessionKey string, origin *SessionSource, sourceValue string, displayName *string, parentSessionID string) *sessionCreateArgs {
	row := state.NewSession{ID: sessionID, Source: sourceValue, SessionKey: sessionKey, ParentSessionID: parentSessionID,
		TransportProfile: TransportProfileOf(origin), OriginJSON: originJSON(origin)}
	if displayName != nil {
		row.DisplayName = *displayName
	}
	if origin != nil {
		row.UserID, row.ChatID, row.ChatType = derefStr(origin.UserID), origin.ChatID, origin.ChatType
		row.ThreadID, row.ProfileName = derefStr(origin.ThreadID), derefStr(origin.Profile)
	}
	if parentSessionID != "" {
		row.ModelConfig = map[string]any{"_reset_from": parentSessionID}
	}
	return &sessionCreateArgs{SessionID: sessionID, Row: row}
}

// Value of an optional string, empty when nil
func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// Inserts the session row and records its routing peer, reporting failures through log
func (s *SessionStore) createSessionRow(ctx context.Context, sessionKey string, create *sessionCreateArgs, origin *SessionSource, displayName *string, log func(error)) {
	db := s.dbForKey(ctx, sessionKey)
	if db == nil {
		log(fmt.Errorf("'NoneType' object has no attribute 'create_session'"))
		return
	}
	if err := db.CreateSession(create.Row); err != nil {
		log(err)
		return
	}
	if err := s.recordGatewaySessionPeer(ctx, create.SessionID, sessionKey, origin, displayName, false, nil); err != nil {
		log(err)
	}
}
