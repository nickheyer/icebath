package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session database surface the store reads and writes, satisfied by the state SessionDB and test doubles
type SessionStoreDB interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	GetMeta(ctx context.Context, key string) (string, bool, error)
	SetMeta(ctx context.Context, key, value string) error
	SessionCountGE(ctx context.Context, n int) (bool, error)
	CreateSession(n state.NewSession) error
	EndSession(sessionID, endReason string) error
	ReopenSession(ctx context.Context, sessionID string) error
	GetCompressionTip(ctx context.Context, sessionID string) (string, error)
	GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error)
	LocalReceipt(ctx context.Context, sessionID string) (*pyjson.Object, error)
	RouteRetired(ctx context.Context, sessionKey, entryJSON string) (bool, error)
	GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error)
	ExecuteWrite(ctx context.Context, fn func(conn state.SQLiteConn) error) error
}

// Routing index loader of hermes_state_gateway load_gateway_routing_entries, key to entry JSON
type gatewayRoutingLoader interface {
	LoadGatewayRoutingEntries(ctx context.Context, scope string) (map[string]string, error)
}

// Whole index rewrite returning the keys skipped as retired routes
type gatewayRoutingReplacer interface {
	ReplaceGatewayRoutingEntries(ctx context.Context, entries map[string]string, scope string) (map[string]bool, error)
}

// Single entry routing upsert
type gatewayRoutingSaver interface {
	SaveGatewayRoutingEntry(ctx context.Context, sessionKey, entryJSON, scope string) error
}

// Durable route retirement with fences in one transaction
type gatewayRouteRetirer interface {
	RetireGatewayRoutes(ctx context.Context, sessionIDs []string, namespace string, expiring []string) error
}

// The state SessionDB carries the whole routing family the store persists through
var (
	_ SessionStoreDB             = (*state.SessionDB)(nil)
	_ gatewayRoutingLoader       = (*state.SessionDB)(nil)
	_ gatewayRoutingReplacer     = (*state.SessionDB)(nil)
	_ gatewayRoutingSaver        = (*state.SessionDB)(nil)
	_ gatewayRouteRetirer        = (*state.SessionDB)(nil)
	_ gatewaySessionPeerRecorder = (*state.SessionDB)(nil)
	_ gatewaySessionPeerFinder   = (*state.SessionDB)(nil)
	_ sessionResetPromoter       = (*state.SessionDB)(nil)
)

const sessionsJSONReadme = "LEGACY MIRROR of the gateway routing index (the primary copy lives in the gateway_routing " +
	"table in ~/.icebath/state.db). Maps messaging session keys (agent:main:<platform>:...) to " +
	"active session IDs. This is NOT the session list. ALL sessions (CLI, TUI, and gateway) live " +
	"in ~/.icebath/state.db and are shown by `icebath sessions list` and `/sessions`. Disable this " +
	"file with `gateway.write_sessions_json: false` in config.yaml."

// True for the test isolation guard that must stay loud and is never cached
func isLiveSystemGuard(err error) bool {
	return err != nil && strings.Contains(err.Error(), "live-system guard")
}

// Publishes a committed retirement by dropping every entry naming the ids
func (s *SessionStore) RetireRuntimeSessions(sessionIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := map[string]bool{}
	for _, sid := range sessionIDs {
		targets[sid] = true
	}
	for _, e := range s.entries.values() {
		if targets[e.SessionID] {
			s.entries.delete(e.SessionKey)
		}
	}
	for sid := range targets {
		delete(s.sessionOwnerHints, sid)
	}
}

// Durably retires the ids in the routing index's own database, failing when it cannot take the fence
func (s *SessionStore) FenceRetiredRoutes(ctx context.Context, sessionIDs []string, namespace string, expiring []string) error {
	retirer, ok := s.routingDB(ctx).(gatewayRouteRetirer)
	if !ok || retirer == nil {
		return errors.New("routing store unavailable for deletion fence")
	}
	return retirer.RetireGatewayRoutes(ctx, sessionIDs, namespace, expiring)
}

// Drops from an existing sessions.json every retired route, only those naming the ids when given
func (s *SessionStore) StripRetiredMirrorRoutes(ctx context.Context, sessionIDs []string) error {
	db := s.routingDB(ctx)
	sessionsFile := filepath.Join(s.SessionsDir, "sessions.json")
	if db == nil {
		return nil
	}
	if _, err := os.Stat(sessionsFile); err != nil {
		return nil
	}
	var targets map[string]bool
	if sessionIDs != nil {
		targets = map[string]bool{}
		for _, sid := range sessionIDs {
			targets[sid] = true
		}
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	data, err := readSessionsJSON(sessionsFile)
	if err != nil {
		return err
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	kept := pyjson.NewObject()
	for _, key := range obj.Keys {
		entry := obj.Vals[key]
		var sid any
		if eo, ok := entry.(*pyjson.Object); ok {
			sid, _ = eo.Get("session_id")
		}
		sidStr, isStr := sid.(string)
		if strings.HasPrefix(key, "_") || !isStr || (targets != nil && !targets[sidStr]) {
			kept.Set(key, entry)
			continue
		}
		retired, err := db.RouteRetired(ctx, key, sessionIDJSON(sidStr))
		if err != nil {
			return err
		}
		if !retired {
			kept.Set(key, entry)
		}
	}
	if kept.Len() < obj.Len() {
		return config.AtomicJSONWriteMode(ctx, sessionsFile, kept, 0o600, pyjson.Indent(2), pyjson.EnsureASCII(false))
	}
	return nil
}

// JSON of a one key session id object as route_retired receives it
func sessionIDJSON(sessionID string) string {
	o := pyjson.NewObject()
	o.Set("session_id", sessionID)
	text, _ := pyjson.Dumps(o)
	return text
}

// Decoded sessions.json read with a leading BOM tolerated
func readSessionsJSON(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	return pyjson.LoadsBytes(raw)
}

// Opens through the process wide registry, a package var so tests substitute the open
var acquireSessionDB = state.Acquire

// Session database of the active scope or the pinned path, cached per path with failed opens backing off
func (s *SessionStore) openSessionDBForActiveScope(ctx context.Context, dbPath string) (SessionStoreDB, error) {
	path := dbPath
	if path == "" {
		path = state.ResolveDefaultDBPath(ctx)
	}
	handle, err := s.dbHandleCache.Get(ctx, path, func() (any, error) {
		db, err := acquireSessionDB(ctx, path)
		if err != nil {
			if !isLiveSystemGuard(err) {
				fmt.Printf("[gateway] Warning: SQLite session store unavailable, falling back to JSONL: %v\n", err)
			}
			return nil, err
		}
		return db, nil
	}, isLiveSystemGuard)
	if err != nil {
		return nil, err
	}
	db, _ := handle.(SessionStoreDB)
	return db, nil
}

// Opened handle or nil, the live system guard staying a loud failure
func (s *SessionStore) openOrNil(ctx context.Context, dbPath string) SessionStoreDB {
	db, err := s.openSessionDBForActiveScope(ctx, dbPath)
	if isLiveSystemGuard(err) {
		panic(err)
	}
	return db
}

// Explicitly pinned database and whether one was pinned
func (s *SessionStore) pinned() (SessionStoreDB, bool) { return s.pinnedDB, s.dbPinned }

// Session database of the active scope unless one was pinned
func (s *SessionStore) db(ctx context.Context) SessionStoreDB {
	if db, ok := s.pinned(); ok {
		return db
	}
	return s.openOrNil(ctx, "")
}

// The one store owning the routing index whatever scope is active
func (s *SessionStore) routingDB(ctx context.Context) SessionStoreDB {
	if db, ok := s.pinned(); ok {
		return db
	}
	if s.routingHome == "" {
		return s.db(ctx)
	}
	db, err := s.openSessionDBForActiveScope(ctx, filepath.Join(s.routingHome, "state.db"))
	if err != nil {
		return nil
	}
	return db
}

// Whether the config multiplexes profiles from one process
func (s *SessionStore) multiplexProfiles() bool {
	return s.Config != nil && s.Config.MultiplexProfiles != nil && *s.Config.MultiplexProfiles
}

// Non default profile owning the key when multiplexing, empty otherwise
func (s *SessionStore) namedProfileForKey(sessionKey string) string {
	if !s.multiplexProfiles() {
		return ""
	}
	profile, ok := profileFromSessionKey(sessionKey)
	if !ok || profile == "" || profile == "default" {
		return ""
	}
	return profile
}

// Home of the profile owning the key, empty when there is no named owner or it does not resolve
func (s *SessionStore) profileHomeForKey(sessionKey string) string {
	profile := s.namedProfileForKey(sessionKey)
	if profile == "" {
		return ""
	}
	s.mu.Lock()
	cached, ok := s.profileHomeCache[profile]
	s.mu.Unlock()
	if ok {
		return cached
	}
	home := ""
	if profiles.ProfileExists(profile) {
		if dir, err := profiles.GetProfileDir(profile); err == nil {
			home = dir
		} else {
			slog.Debug(fmt.Sprintf("Could not resolve profile home for %q: %v", sessionKey, err))
		}
	}
	if home != "" {
		s.mu.Lock()
		s.profileHomeCache[profile] = home
		s.mu.Unlock()
	}
	return home
}

// Session database holding the key's rows whatever scope is active, nil when the owner cannot resolve
func (s *SessionStore) dbForKey(ctx context.Context, sessionKey string) SessionStoreDB {
	if db, ok := s.pinned(); ok {
		return db
	}
	profile := s.namedProfileForKey(sessionKey)
	if profile == "" {
		if s.routingHome == "" || !s.multiplexProfiles() {
			return s.db(ctx)
		}
		db, err := s.openSessionDBForActiveScope(ctx, filepath.Join(s.routingHome, "state.db"))
		if err != nil {
			return nil
		}
		return db
	}
	home := s.profileHomeForKey(sessionKey)
	if home == "" {
		slog.Warn(fmt.Sprintf("gateway.session: profile %q has no resolvable home (key %q); refusing to fall back to the ambient store", profile, sessionKey))
		return nil
	}
	db, err := s.openSessionDBForActiveScope(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		return nil
	}
	return db
}

// Routing key owning the session id from the index or a pre published hint, read without the lock
func (s *SessionStore) ownerKeyForSessionID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	for _, e := range s.entries.values() {
		if e.SessionID == sessionID {
			return e.SessionKey
		}
	}
	return s.sessionOwnerHints[sessionID]
}

// Session database holding the id's row, unknown ids reading the ambient store
func (s *SessionStore) dbForSessionID(ctx context.Context, sessionID string) SessionStoreDB {
	if sessionID == "" {
		return s.db(ctx)
	}
	return s.dbForKey(ctx, s.ownerKeyForSessionID(sessionID))
}

// Closes every session database handle this store opened
func (s *SessionStore) CloseAllDBHandles() {
	s.dbHandleCache.CloseAll(func(handle any) {
		if db, ok := handle.(*state.SessionDB); ok {
			state.ReleaseOrClose(db)
		}
	})
}

// Loads the index from disk once
func (s *SessionStore) ensureLoaded(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
}

// Loads the index and returns the key's entry, lock held
func (s *SessionStore) entryLocked(ctx context.Context, sessionKey string) *SessionEntry {
	s.ensureLoadedLocked(ctx)
	return s.entries.get(sessionKey)
}

// Namespace of this store's routing rows, the resolved sessions dir
func (s *SessionStore) routingScope() string {
	if abs, err := filepath.Abs(s.SessionsDir); err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved
		}
		return abs
	}
	return s.SessionsDir
}

// Meta key holding a chat's current labels
func chatLabelsMetaKey(platform Platform, scopeID *string, chatID string) string {
	scope := ""
	if scopeID != nil {
		scope = *scopeID
	}
	return fmt.Sprintf("gateway_chat_labels:%s:%s:%s", string(platform), scope, chatID)
}

// Persists the source's chat name and topic as observed now, false when no database handle exists
func (s *SessionStore) RecordChatLabels(ctx context.Context, source *SessionSource) (bool, error) {
	db := s.routingDB(ctx)
	if db == nil {
		return false, nil
	}
	labels, err := pyjson.Dumps([]any{optStr(source.ChatName), optStr(source.ChatTopic)})
	if err != nil {
		return false, err
	}
	if err := db.SetMeta(ctx, chatLabelsMetaKey(source.Platform, source.ScopeID, source.ChatID), labels); err != nil {
		return false, err
	}
	return true, nil
}

// Chat name and topic last recorded, ok false when none was, an error when the store is unreadable
func (s *SessionStore) ChatLabels(ctx context.Context, platform Platform, scopeID *string, chatID string) (chatName, chatTopic any, ok bool, err error) {
	db := s.routingDB(ctx)
	if db == nil {
		return nil, nil, false, &PyError{Kind: "LookupError", Msg: "session database unavailable"}
	}
	raw, found, err := db.GetMeta(ctx, chatLabelsMetaKey(platform, scopeID, chatID))
	if err != nil {
		return nil, nil, false, err
	}
	if !found || raw == "" {
		return nil, nil, false, nil
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		return nil, nil, false, err
	}
	pair, isList := parsed.([]any)
	if !isList || len(pair) != 2 {
		return nil, nil, false, &PyError{Kind: "ValueError", Msg: "chat labels are not a pair"}
	}
	return pair[0], pair[1], true, nil
}

// Loads state.db routing rows into the index, false with no loader or a failed load, lock held
func (s *SessionStore) loadRoutingRowsLocked(ctx context.Context) bool {
	loader, ok := s.routingDB(ctx).(gatewayRoutingLoader)
	if !ok || loader == nil {
		return false
	}
	rows, err := loader.LoadGatewayRoutingEntries(ctx, s.routingScope())
	if err != nil {
		slog.Warn(fmt.Sprintf("gateway.session: state.db routing load failed: %v", err))
		return false
	}
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		if entry := routingEntryFromJSON(ctx, key, rows[key]); entry != nil {
			s.entries.set(key, entry)
		}
	}
	return true
}

// One parsed routing row, nil with a warning when invalid
func routingEntryFromJSON(ctx context.Context, key, entryJSON string) *SessionEntry {
	parsed, err := pyjson.Loads(entryJSON)
	if err != nil {
		slog.Warn(fmt.Sprintf("Skipping invalid routing entry %q: %v", key, err))
		return nil
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil
	}
	entry, err := SessionEntryFromDict(ctx, obj)
	if err != nil {
		slog.Warn(fmt.Sprintf("Skipping invalid routing entry %q: %v", key, err))
		return nil
	}
	return entry
}

// Loads the routing index with state.db primary and sessions.json filling missing keys, lock held
func (s *SessionStore) ensureLoadedLocked(ctx context.Context) {
	if s.loaded {
		s.reconcileRecoveredRoutingLocked(ctx)
		return
	}
	_ = os.MkdirAll(s.SessionsDir, 0o755)
	dbLoaded := s.loadRoutingRowsLocked(ctx)
	dbHadEntries := dbLoaded && s.entries.len() > 0
	mirrorRetired := s.importLegacySessionsJSON(ctx, dbHadEntries)
	s.loaded, s.routingDBLoaded = true, dbLoaded
	s.healMissingOrigins(ctx, s.entries.values())
	if dbLoaded {
		s.routingFallbackBaseline = nil
	} else {
		s.routingFallbackBaseline = s.entriesAsDicts()
	}
	if mirrorRetired > 0 && dbLoaded {
		if err := s.save(ctx); err != nil {
			slog.Warn(fmt.Sprintf("gateway.session: routing save after retired import failed: %v", err))
		}
		if !s.writeSessionsJSON {
			if err := s.StripRetiredMirrorRoutes(ctx, nil); err != nil {
				slog.Warn(fmt.Sprintf("gateway.session: could not strip retired routes from sessions.json: %v", err))
			}
		}
	}
	s.pruneStaleSessionsLocked(ctx)
}

// Fills a missing origin from the entry's conversation row
func (s *SessionStore) healMissingOrigins(ctx context.Context, entries []*SessionEntry) {
	for _, e := range entries {
		if e.Origin == nil {
			e.Origin = s.storedOrigin(ctx, e)
		}
	}
}

// Messaging source rebuilt from the entry's row when it still routes to the entry's key
func (s *SessionStore) storedOrigin(ctx context.Context, entry *SessionEntry) *SessionSource {
	db := s.dbForKey(ctx, entry.SessionKey)
	if db == nil {
		return nil
	}
	row, err := db.GetSession(ctx, entry.SessionID)
	if err != nil || row == nil {
		return nil
	}
	var label any = string(entry.Platform)
	if entry.Platform == "" {
		label, _ = row.Get("source")
	}
	platform, ok := messagingPlatform(ctx, label)
	if !ok {
		return nil
	}
	source := rowSource(ctx, row, platform, s.namedProfileForKey(entry.SessionKey))
	if source == nil || source.Platform != platform {
		return nil
	}
	if key, err := s.generateSessionKey(ctx, source, nil); err != nil || key != entry.SessionKey {
		return nil
	}
	return source
}

// Imports sessions.json entries the database lacks and returns how many it skipped as retired, lock held
func (s *SessionStore) importLegacySessionsJSON(ctx context.Context, dbHadEntries bool) int {
	sessionsFile := filepath.Join(s.SessionsDir, "sessions.json")
	retired := 0
	if _, err := os.Stat(sessionsFile); err != nil {
		return retired
	}
	data, err := readSessionsJSON(sessionsFile)
	if err != nil {
		fmt.Printf("[gateway] Warning: Failed to load sessions: %v\n", err)
		return retired
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		fmt.Printf("[gateway] Warning: Failed to load sessions: %s object has no attribute 'items'\n", jsonTypeName(data))
		return retired
	}
	imported := 0
	for _, key := range obj.Keys {
		if strings.HasPrefix(key, "_") || s.entries.has(key) {
			continue
		}
		entryData, ok := obj.Vals[key].(*pyjson.Object)
		if !ok {
			slog.Warn(fmt.Sprintf("Skipping invalid session entry %q: expected dict, got %s", key, jsonTypeName(obj.Vals[key])))
			continue
		}
		sid, hasID := entryData.Get("session_id")
		if !hasID {
			slog.Warn(fmt.Sprintf("Skipping invalid session entry %q: 'session_id'", key))
			continue
		}
		if db := s.routingDB(ctx); db != nil {
			skip, err := db.RouteRetired(ctx, key, sessionIDJSON(pyStrOf(sid)))
			if err != nil {
				fmt.Printf("[gateway] Warning: Failed to load sessions: %v\n", err)
				return retired
			}
			if skip {
				retired++
				continue
			}
		}
		entry, err := SessionEntryFromDict(ctx, entryData)
		if err != nil {
			slog.Warn(fmt.Sprintf("Skipping invalid session entry %q: %v", key, err))
			continue
		}
		s.entries.set(key, entry)
		imported++
	}
	if imported > 0 && dbHadEntries {
		plural := "ies"
		if imported == 1 {
			plural = "y"
		}
		slog.Info(fmt.Sprintf("gateway.session: imported %d legacy sessions.json entr%s missing from state.db routing table", imported, plural))
	}
	return retired
}

// Python type name of a decoded JSON value
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int64:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Removes entries whose session ended in state.db at startup, repointing recoverable ones, lock held
func (s *SessionStore) pruneStaleSessionsLocked(ctx context.Context) {
	if s.entries.len() == 0 {
		return
	}
	var stale []string
	recovered := 0
	for _, key := range s.entries.keyList() {
		entry := s.entries.get(key)
		db := s.dbForKey(ctx, key)
		if db == nil {
			continue
		}
		row, err := db.GetSession(ctx, entry.SessionID)
		if err != nil {
			slog.Warn(fmt.Sprintf("gateway.session: stale-entry pruning skipped due to DB error: %v", err))
			return
		}
		if row == nil {
			continue
		}
		if end, _ := row.Get("end_reason"); end == nil {
			continue
		}
		prune, replacement := s.staleEntryVerdict(ctx, key, entry, row)
		if prune {
			stale = append(stale, key)
		} else if replacement != nil {
			s.entries.set(key, replacement)
			recovered++
		}
	}
	for _, key := range stale {
		s.entries.delete(key)
	}
	if len(stale) > 0 || recovered > 0 {
		if err := s.save(ctx); err != nil {
			slog.Warn(fmt.Sprintf("gateway.session: stale-entry pruning skipped due to DB error: %v", err))
		}
	}
}

// Verdict for an entry whose row ended, prune, a replacement entry, or keep as is
func (s *SessionStore) staleEntryVerdict(ctx context.Context, key string, entry *SessionEntry, row *pyjson.Object) (bool, *SessionEntry) {
	endReason, _ := row.Get("end_reason")
	if target := s.localReceiptTarget(ctx, entry); target != "" {
		if target == entry.SessionID {
			return false, nil
		}
		repointed := *entry
		repointed.SessionID = target
		slog.Warn(fmt.Sprintf("gateway.session: repointing stale sessions.json entry %q from ended %s (end_reason=%s) to its local receipt target %s",
			key, entry.SessionID, pyReprValue(endReason), target))
		return false, &repointed
	}
	var recoveredEntry *SessionEntry
	if entry.Origin != nil {
		var err error
		recoveredEntry, err = s.recoverSessionFromDB(ctx, key, entry.Origin, sessionNow(), true)
		if err != nil {
			slog.Debug(fmt.Sprintf("gateway.session: recovery lookup failed for stale sessions.json entry %q -> %s: %v", key, entry.SessionID, err))
			return false, nil
		}
	}
	if recoveredEntry != nil && recoveredEntry.SessionID != entry.SessionID {
		slog.Warn(fmt.Sprintf("gateway.session: repointing stale sessions.json entry %q from ended %s (end_reason=%s) to recovered %s",
			key, entry.SessionID, pyReprValue(endReason), recoveredEntry.SessionID))
		if entry.PromptPin != nil && entry.PromptPin.Len() > 0 && s.compressionTipForSessionID(ctx, entry.SessionID) == recoveredEntry.SessionID {
			recoveredEntry.PromptPin = entry.PromptPin
		}
		return false, recoveredEntry
	}
	if recoveredEntry != nil {
		slog.Info(fmt.Sprintf("gateway.session: reopened ended session %s for sessions.json entry %q (end_reason=%s); keeping route",
			entry.SessionID, key, pyReprValue(endReason)))
		return false, nil
	}
	slog.Warn(fmt.Sprintf("gateway.session: pruning stale sessions.json entry %q -> %s (end_reason=%s); left by a crashed gateway",
		key, entry.SessionID, pyReprValue(endReason)))
	return true, nil
}

// Serializable snapshot of the index, lock held
func (s *SessionStore) entriesAsDicts() *pyjson.Object {
	out := pyjson.NewObject()
	for _, e := range s.entries.values() {
		out.Set(e.SessionKey, e.ToDict())
	}
	return out
}

// Persists the routing index while the caller holds the lock
func (s *SessionStore) save(ctx context.Context) error {
	data, generation := s.snapshotRoutingLocked(ctx)
	return s.persistRoutingData(ctx, data, generation)
}

// Bumps the routing generation shared by full and single entry saves, lock held
func (s *SessionStore) nextRoutingGenerationLocked() int {
	s.routingGeneration++
	return s.routingGeneration
}

// Merges authoritative rows after a fallback only startup load, lock held
func (s *SessionStore) reconcileRecoveredRoutingLocked(ctx context.Context) {
	baseline := s.routingFallbackBaseline
	if s.routingDBLoaded || baseline == nil {
		return
	}
	loader, ok := s.routingDB(ctx).(gatewayRoutingLoader)
	if !ok || loader == nil {
		return
	}
	durable, err := loader.LoadGatewayRoutingEntries(ctx, s.routingScope())
	if err != nil {
		slog.Warn(fmt.Sprintf("gateway.session: recovered state.db routing load failed: %v", err))
		return
	}
	current := s.entriesAsDicts()
	for _, key := range slices.Sorted(maps.Keys(durable)) {
		durableEntry := routingEntryFromJSON(ctx, key, durable[key])
		if durableEntry == nil {
			continue
		}
		base, inBaseline := baseline.Get(key)
		cur, inCurrent := current.Get(key)
		switch {
		case !inBaseline:
			if !s.entries.has(key) {
				s.entries.set(key, durableEntry)
			}
		case !inCurrent:
		case pyjson.Equal(cur, base):
			s.entries.set(key, durableEntry)
		}
	}
	s.healMissingOrigins(ctx, s.entries.values())
	s.routingDBLoaded, s.routingFallbackBaseline = true, nil
}

// Immutable routing data with a fresh generation, lock held
func (s *SessionStore) snapshotRoutingLocked(ctx context.Context) (*pyjson.Object, int) {
	s.reconcileRecoveredRoutingLocked(ctx)
	return s.entriesAsDicts(), s.nextRoutingGenerationLocked()
}

// Serializes whole index writers through the save lock folding in newer fast upserts
func (s *SessionStore) persistRoutingData(ctx context.Context, data *pyjson.Object, generation int) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if generation <= s.persistedRoutingGeneration {
		return nil
	}
	for key, fast := range s.fastPersistedEntries {
		if fast.revision > generation {
			if parsed, err := pyjson.Loads(fast.entryJSON); err == nil {
				data.Set(key, parsed)
			}
		}
	}
	dbSaved := false
	if replacer, ok := s.routingDB(ctx).(gatewayRoutingReplacer); ok && replacer != nil {
		encoded := map[string]string{}
		for _, k := range data.Keys {
			text, err := pyjson.Dumps(data.Vals[k])
			if err != nil {
				return err
			}
			encoded[k] = text
		}
		retired, err := replacer.ReplaceGatewayRoutingEntries(ctx, encoded, s.routingScope())
		if err != nil {
			slog.Warn(fmt.Sprintf("gateway.session: state.db routing save failed: %v", err))
		} else {
			dbSaved = true
			if len(retired) > 0 {
				data = data.Copy()
				for k := range retired {
					data.Delete(k)
				}
			}
		}
	}
	if s.writeSessionsJSON || !dbSaved {
		if err := s.saveSessionsJSON(data); err != nil {
			if !dbSaved {
				return err
			}
			slog.Warn(fmt.Sprintf("gateway.session: sessions.json mirror save failed after state.db commit: %v", err))
		}
	}
	s.persistedRoutingGeneration = generation
	for key, fast := range s.fastPersistedEntries {
		if fast.revision <= generation {
			delete(s.fastPersistedEntries, key)
		}
	}
	return nil
}

// Writes the legacy sessions.json mirror atomically behind its readme sentinel
func (s *SessionStore) saveSessionsJSON(data *pyjson.Object) error {
	out := pyjson.NewObject()
	out.Set("_README", sessionsJSONReadme)
	for _, k := range data.Keys {
		out.Set(k, data.Vals[k])
	}
	return config.AtomicJSONWriteMode(s.ctx, filepath.Join(s.SessionsDir, "sessions.json"), out, 0o600, pyjson.Indent(2), pyjson.EnsureASCII(false))
}

// Snapshots under the lock and persists after releasing it
func (s *SessionStore) saveEntries(ctx context.Context) error {
	s.mu.Lock()
	data, generation := s.snapshotRoutingLocked(ctx)
	s.mu.Unlock()
	return s.persistRoutingData(ctx, data, generation)
}

// Persists one entry by upsert, a candidate durable before publication, falling back to a full rewrite
func (s *SessionStore) saveEntry(ctx context.Context, sessionKey string, entryData *pyjson.Object, lockHeld bool) error {
	if !lockHeld {
		s.mu.Lock()
	}
	entry := s.entries.get(sessionKey)
	if entry == nil {
		if !lockHeld {
			s.mu.Unlock()
		}
		return nil
	}
	serialized := entryData
	if serialized == nil {
		serialized = entry.ToDict()
	}
	entryJSON, err := pyjson.Dumps(serialized)
	revision := s.nextRoutingGenerationLocked()
	if !lockHeld {
		s.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if saver, ok := s.routingDB(ctx).(gatewayRoutingSaver); ok && saver != nil {
		saveErr := func() error {
			s.saveMu.Lock()
			defer s.saveMu.Unlock()
			if s.persistedRoutingGeneration >= revision {
				return nil
			}
			fastPersisted := lazyMap(&s.fastPersistedEntries)
			if persisted, ok := fastPersisted[sessionKey]; ok && persisted.revision >= revision {
				return nil
			}
			if err := saver.SaveGatewayRoutingEntry(ctx, sessionKey, entryJSON, s.routingScope()); err != nil {
				return err
			}
			fastPersisted[sessionKey] = fastPersistedEntry{revision: revision, entryJSON: entryJSON}
			return nil
		}()
		if saveErr == nil {
			return nil
		}
		slog.Warn(fmt.Sprintf("gateway.session: single-entry routing save failed for %q (%v); falling back to full index rewrite", sessionKey, saveErr))
	}
	if entryData != nil {
		if !lockHeld {
			s.mu.Lock()
		}
		fallback := s.entriesAsDicts()
		if !lockHeld {
			s.mu.Unlock()
		}
		fallback.Set(sessionKey, entryData.Copy())
		return s.persistRoutingData(ctx, fallback, revision)
	}
	if lockHeld {
		return s.save(ctx)
	}
	return s.saveEntries(ctx)
}
