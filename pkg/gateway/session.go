package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// True when the value could traverse outside the sessions dir, strict for ids and lenient for keys
func isPathUnsafe(value string, strict bool) bool {
	if value == "" {
		return false
	}
	if strings.Contains(value, "..") || (strict && (strings.Contains(value, "/") || strings.Contains(value, "\\"))) {
		return true
	}
	if !strict && (strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\")) {
		return true
	}
	rs := []rune(value)
	return len(rs) >= 2 && unicode.IsLetter(rs[0]) && rs[1] == ':'
}

var PersistableModelOverrideKeys = []string{"model", "provider", "base_url"}

// Copy of a dict override with only persistable non empty keys as strings, nil when nothing is left
func SanitizeModelOverride(override any) *pyjson.Object {
	var keys []string
	var value func(key string) any
	switch obj := override.(type) {
	case *pyjson.Object:
		if obj == nil {
			return nil
		}
		keys, value = obj.Keys, func(key string) any { return obj.Vals[key] }
	case map[string]any:
		keys, value = PersistableModelOverrideKeys, func(key string) any { return obj[key] }
	default:
		return nil
	}
	cleaned := pyjson.NewObject()
	for _, k := range keys {
		v := value(k)
		if !slices.Contains(PersistableModelOverrideKeys, k) || v == nil || v == "" {
			continue
		}
		cleaned.Set(k, pyStrOf(v))
	}
	if cleaned.Len() == 0 {
		return nil
	}
	return cleaned
}

// Python str of a decoded JSON value
func pyStrOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	if f, ok := v.(float64); ok {
		return pyFloatStr(f)
	}
	if _, ok := v.(*pyjson.Object); ok {
		text, _ := pyjson.Dumps(v)
		return text
	}
	return fmt.Sprint(v)
}

// Routing index entry mapping a session key to its current session id and metadata
type SessionEntry struct {
	SessionKey          string
	SessionID           string
	CreatedAt           pyDatetime
	UpdatedAt           pyDatetime
	Origin              *SessionSource
	DisplayName         *string
	Platform            Platform
	ChatType            string
	Metadata            *pyjson.Object
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
	TotalTokens         int64
	EstimatedCostUSD    float64
	CostStatus          string
	LastPromptTokens    int64
	WasAutoReset        bool
	AutoResetReason     *string
	ResetHadActivity    bool
	PrevSessionID       *string
	IsFreshReset        bool
	ExpiryFinalized     bool
	Suspended           bool
	ResumePending       bool
	ResumeReason        *string
	LastResumeMarkedAt  *pyDatetime
	ActiveTurnToken     *string
	ActiveTurnStartedAt *pyDatetime
	ModelOverride       *pyjson.Object
	TransportProfile    *string
	PromptPin           *pyjson.Object
	Yolo                *bool
}

// Entry with the dataclass defaults for the given key, id and timestamps
func NewSessionEntry(sessionKey, sessionID string, createdAt, updatedAt pyDatetime) *SessionEntry {
	return &SessionEntry{SessionKey: sessionKey, SessionID: sessionID, CreatedAt: createdAt, UpdatedAt: updatedAt,
		ChatType: "dm", Metadata: pyjson.NewObject(), CostStatus: "unknown"}
}

// Nil for an empty optional string, else a pointer to it
func optStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// Wire form of the entry in hermes key order
func (e *SessionEntry) ToDict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("session_key", e.SessionKey)
	out.Set("session_id", e.SessionID)
	out.Set("created_at", e.CreatedAt.isoformat())
	out.Set("updated_at", e.UpdatedAt.isoformat())
	out.Set("display_name", optStr(e.DisplayName))
	if e.Platform != "" {
		out.Set("platform", string(e.Platform))
	} else {
		out.Set("platform", nil)
	}
	out.Set("chat_type", e.ChatType)
	meta := e.Metadata
	if meta == nil {
		meta = pyjson.NewObject()
	}
	out.Set("metadata", meta)
	out.Set("input_tokens", e.InputTokens)
	out.Set("output_tokens", e.OutputTokens)
	out.Set("cache_read_tokens", e.CacheReadTokens)
	out.Set("cache_write_tokens", e.CacheWriteTokens)
	out.Set("total_tokens", e.TotalTokens)
	out.Set("last_prompt_tokens", e.LastPromptTokens)
	out.Set("estimated_cost_usd", e.EstimatedCostUSD)
	out.Set("cost_status", e.CostStatus)
	out.Set("expiry_finalized", e.ExpiryFinalized)
	out.Set("suspended", e.Suspended)
	out.Set("resume_pending", e.ResumePending)
	out.Set("resume_reason", optStr(e.ResumeReason))
	out.Set("yolo", e.Yolo != nil && *e.Yolo)
	if e.Yolo == nil {
		out.Set("yolo_toggle", nil)
	} else {
		out.Set("yolo_toggle", *e.Yolo)
	}
	out.Set("last_resume_marked_at", isoOf(e.LastResumeMarkedAt))
	out.Set("active_turn_token", optStr(e.ActiveTurnToken))
	out.Set("active_turn_started_at", isoOf(e.ActiveTurnStartedAt))
	out.Set("is_fresh_reset", e.IsFreshReset)
	out.Set("was_auto_reset", e.WasAutoReset)
	out.Set("auto_reset_reason", optStr(e.AutoResetReason))
	out.Set("reset_had_activity", e.ResetHadActivity)
	out.Set("prev_session_id", optStr(e.PrevSessionID))
	if e.ModelOverride != nil && e.ModelOverride.Len() > 0 {
		out.Set("model_override", SanitizeModelOverride(e.ModelOverride))
	}
	if e.PromptPin != nil && e.PromptPin.Len() > 0 {
		if pin := SanitizePromptPin(e.PromptPin); pin != nil {
			out.Set("prompt_pin", pin)
		}
	}
	if e.TransportProfile != nil && *e.TransportProfile != "" {
		out.Set("transport_profile", *e.TransportProfile)
	}
	if e.Origin != nil {
		out.Set("origin", e.Origin.ToDict())
	}
	return out
}

// Error raised by SessionEntryFromDict for a malformed or unsafe entry
type SessionEntryError struct{ Kind, Msg string }

func (e *SessionEntryError) Error() string { return e.Msg }

// Integer field of a decoded entry, the default when absent
func entryInt(data *pyjson.Object, key string) (int64, error) {
	v, ok := data.Get(key)
	if !ok {
		return 0, nil
	}
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &SessionEntryError{Kind: "TypeError", Msg: fmt.Sprintf("invalid %s value", key)}
}

// Truthiness of a decoded entry field, false when absent
func entryBool(data *pyjson.Object, key string, def bool) bool {
	v, ok := data.Get(key)
	if !ok {
		return def
	}
	return pyTruthy(v)
}

// Optional string field of a decoded entry, nil when absent or null
func entryOptStr(data *pyjson.Object, key string) *string {
	v, ok := data.Get(key)
	if !ok || v == nil {
		return nil
	}
	s := pyStrOf(v)
	return &s
}

// Entry rebuilt from its wire form, rejecting traversal ids and keys
func SessionEntryFromDict(ctx context.Context, data *pyjson.Object) (*SessionEntry, error) {
	var origin *SessionSource
	if raw, _ := data.Get("origin"); raw != nil {
		if obj, ok := raw.(*pyjson.Object); ok {
			src, err := SessionSourceFromDict(ctx, pyjson.Plain(obj).(map[string]any))
			if err != nil {
				return nil, err
			}
			origin = src
		}
	}
	var platform Platform
	if raw, _ := data.Get("platform"); pyTruthy(raw) {
		if p, ok := platformMember(pyStrOf(raw)); ok {
			platform = p
		} else {
			slog.Debug(fmt.Sprintf("Unknown platform value %v", raw))
		}
	}
	tokenRaw, _ := data.Get("active_turn_token")
	startedRaw, _ := data.Get("active_turn_started_at")
	startedAt := parseISOValue(startedRaw)
	var token *string
	if s, ok := tokenRaw.(string); ok && s != "" {
		token = &s
	} else {
		startedAt = nil
	}
	keyRaw, ok := data.Get("session_key")
	if !ok {
		return nil, &SessionEntryError{Kind: "KeyError", Msg: "'session_key'"}
	}
	idRaw, ok := data.Get("session_id")
	if !ok {
		return nil, &SessionEntryError{Kind: "KeyError", Msg: "'session_id'"}
	}
	sessionKey, sessionID := pyStrOf(keyRaw), pyStrOf(idRaw)
	if isPathUnsafe(sessionID, true) {
		return nil, &SessionEntryError{Kind: "ValueError", Msg: "Invalid session_id: potential directory traversal detected"}
	}
	if isPathUnsafe(sessionKey, false) {
		return nil, &SessionEntryError{Kind: "ValueError", Msg: "Invalid session_key: potential directory traversal detected"}
	}
	created, err := entryTime(data, "created_at")
	if err != nil {
		return nil, err
	}
	updated, err := entryTime(data, "updated_at")
	if err != nil {
		return nil, err
	}
	e := NewSessionEntry(sessionKey, sessionID, created, updated)
	e.Origin, e.Platform = origin, platform
	e.DisplayName = entryOptStr(data, "display_name")
	if v, ok := data.Get("chat_type"); ok {
		e.ChatType = pyStrOf(v)
	}
	if v, _ := data.Get("metadata"); pyTruthy(v) {
		obj, ok := v.(*pyjson.Object)
		if !ok {
			return nil, &SessionEntryError{Kind: "TypeError", Msg: "metadata is not a mapping"}
		}
		e.Metadata = obj.Copy()
	}
	for _, f := range []struct {
		key string
		dst *int64
	}{{"input_tokens", &e.InputTokens}, {"output_tokens", &e.OutputTokens}, {"cache_read_tokens", &e.CacheReadTokens},
		{"cache_write_tokens", &e.CacheWriteTokens}, {"total_tokens", &e.TotalTokens}, {"last_prompt_tokens", &e.LastPromptTokens}} {
		if *f.dst, err = entryInt(data, f.key); err != nil {
			return nil, err
		}
	}
	if v, ok := data.Get("estimated_cost_usd"); ok {
		switch x := v.(type) {
		case float64:
			e.EstimatedCostUSD = x
		case int64:
			e.EstimatedCostUSD = float64(x)
		}
	}
	if v, ok := data.Get("cost_status"); ok {
		e.CostStatus = pyStrOf(v)
	}
	if _, ok := data.Get("expiry_finalized"); ok {
		e.ExpiryFinalized = entryBool(data, "expiry_finalized", false)
	} else {
		e.ExpiryFinalized = entryBool(data, "memory_flushed", false)
	}
	e.Suspended = entryBool(data, "suspended", false)
	e.ResumePending = entryBool(data, "resume_pending", false)
	e.ResumeReason = entryOptStr(data, "resume_reason")
	e.IsFreshReset = entryBool(data, "is_fresh_reset", false)
	e.WasAutoReset = entryBool(data, "was_auto_reset", false)
	e.AutoResetReason = entryOptStr(data, "auto_reset_reason")
	e.ResetHadActivity = entryBool(data, "reset_had_activity", false)
	e.PrevSessionID = entryOptStr(data, "prev_session_id")
	if v, ok := data.Get("yolo_toggle"); ok {
		if b, isBool := v.(bool); isBool {
			e.Yolo = &b
		} else if v != nil {
			b := pyTruthy(v)
			e.Yolo = &b
		}
	} else if v, _ := data.Get("yolo"); v == true {
		b := true
		e.Yolo = &b
	}
	lastMarked, _ := data.Get("last_resume_marked_at")
	e.LastResumeMarkedAt = parseISOValue(lastMarked)
	e.ActiveTurnToken, e.ActiveTurnStartedAt = token, startedAt
	override, _ := data.Get("model_override")
	e.ModelOverride = SanitizeModelOverride(override)
	pin, _ := data.Get("prompt_pin")
	e.PromptPin = SanitizePromptPin(pin)
	if tp, _ := data.Get("transport_profile"); tp != nil {
		if s, ok := tp.(string); ok && s != "" {
			e.TransportProfile = &s
		}
	}
	return e, nil
}

// Required isoformat timestamp field of a decoded entry
func entryTime(data *pyjson.Object, key string) (pyDatetime, error) {
	raw, ok := data.Get(key)
	if !ok {
		return pyDatetime{}, &SessionEntryError{Kind: "KeyError", Msg: "'" + key + "'"}
	}
	s, ok := raw.(string)
	if !ok {
		return pyDatetime{}, &SessionEntryError{Kind: "TypeError", Msg: "fromisoformat: argument must be str"}
	}
	d, err := fromISOFormat(s)
	if err != nil {
		return pyDatetime{}, &SessionEntryError{Kind: "ValueError", Msg: err.Error()}
	}
	return d, nil
}

// One line hint pointing a Slack or Discord lane at its auto reset predecessor, empty otherwise
func BuildChannelContinuityNote(entry *SessionEntry, source *SessionSource) string {
	if source.Platform != PlatformSlack && source.Platform != PlatformDiscord {
		return ""
	}
	if !entry.ResetHadActivity || entry.PrevSessionID == nil || *entry.PrevSessionID == "" {
		return ""
	}
	where := "channel"
	if source.ThreadID != nil && *source.ThreadID != "" {
		where = "thread"
	}
	return fmt.Sprintf("[System note: This %s had an earlier Hermes session (session_id: %s) that was "+
		"auto-reset. If the user refers to earlier work here, or the request depends on this "+
		"%s's history, use the session_search tool to recall that prior session before "+
		"acting — do not assume an unrelated recent session is the right context.]", where, *entry.PrevSessionID, where)
}

// Insertion ordered routing entries, the order a Python dict keeps, readable without the store lock
type sessionEntries struct {
	mu   sync.RWMutex
	keys []string
	m    map[string]*SessionEntry
}

func newSessionEntries() *sessionEntries { return &sessionEntries{m: map[string]*SessionEntry{}} }

func (o *sessionEntries) get(key string) *SessionEntry {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.m[key]
}

func (o *sessionEntries) has(key string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, ok := o.m[key]
	return ok
}

func (o *sessionEntries) len() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.keys)
}

// Stores the entry keeping the position of an existing key
func (o *sessionEntries) set(key string, e *SessionEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.m[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.m[key] = e
}

// Removes the key returning its entry, nil when absent
func (o *sessionEntries) delete(key string) *SessionEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.m[key]
	if !ok {
		return nil
	}
	delete(o.m, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
	return e
}

func (o *sessionEntries) keyList() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return slices.Clone(o.keys)
}

func (o *sessionEntries) values() []*SessionEntry {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*SessionEntry, 0, len(o.keys))
	for _, k := range o.keys {
		out = append(out, o.m[k])
	}
	return out
}

// Session id the key's entry names, read without the store lock
func (o *sessionEntries) sessionIDOf(key string) (string, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	e := o.m[key]
	if e == nil {
		return "", false
	}
	return e.SessionID, true
}

// Points a stored entry at another session id where a reader without the store lock sees it whole
func (o *sessionEntries) retarget(e *SessionEntry, sessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e.SessionID = sessionID
}

// Single flight slot shared by overlapping lookups of one routing key
type sessionFlight struct {
	done   chan struct{}
	result *SessionEntry
	err    error
}

// Lock free results for an existing route snapshotted before the apply phase
type routeChecks struct {
	sessionID   string
	canonicalID string
	isStale     bool
	resetReason string
}

// What the locked apply phase decided for one routing transition
type routeDecision struct {
	entry            *SessionEntry
	needsSave        bool
	metadataOnlySave bool
	needsRecover     bool
	resetReason      *string
	resetHadActivity bool
	prevSessionID    *string
}

// Records that the ended entry is auto reset for the reason
func (d *routeDecision) scheduleReset(reason string, ended *SessionEntry, hadActivity bool) {
	sid := ended.SessionID
	d.resetReason, d.resetHadActivity, d.prevSessionID = &reason, hadActivity, &sid
}

type fastPersistedEntry struct {
	revision  int
	entryJSON string
}

// Gateway routing index and transcripts over SQLite with the sessions.json mirror
type SessionStore struct {
	SessionsDir string
	Config      *GatewayConfig
	// Called with the key, session id and origin once a route names another conversation
	RouteNamed func(sessionKey, sessionID string, origin *SessionSource)

	ctx                        context.Context
	mu                         sync.Mutex
	saveMu                     sync.Mutex
	entries                    *sessionEntries
	loaded                     bool
	routingDBLoaded            bool
	routingFallbackBaseline    *pyjson.Object
	routingGeneration          int
	persistedRoutingGeneration int
	fastPersistedEntries       map[string]fastPersistedEntry
	inflightMu                 sync.Mutex
	inflightSessions           map[string]*sessionFlight
	rowsPending                map[string]bool
	legacySlackClaimMu         sync.Mutex
	claimedLegacySlackKeys     map[string]bool
	reroutesMu                 sync.Mutex
	transcriptReroutes         map[string]string
	hasActiveProcessesFn       func(sessionKey string) (bool, error)
	writeSessionsJSON          bool
	dbPinned                   bool
	localAuthorityEpoch        *int64
	localAuthorityEpochs       map[string]int64
	pinnedDB                   SessionStoreDB
	dbHandleCache              *RecoverableHandleCache
	transcriptRetryMu          sync.Mutex
	transcriptDrainMu          sync.Mutex
	dirtyTranscripts           map[string]*transcriptQueue
	transcriptAppendFailures   map[string]int
	ftsRebuildLastAttemptAt    *float64
	spooledDropSessions        map[string]bool
	profileHomeCache           map[string]string
	sessionOwnerHints          map[string]string
	routingHome                string
}

// Store over the sessions dir priming the state database handle of the active scope
func NewSessionStore(ctx context.Context, sessionsDir string, config *GatewayConfig, hasActiveProcessesFn func(sessionKey string) (bool, error)) *SessionStore {
	s := &SessionStore{SessionsDir: sessionsDir, Config: config, ctx: context.WithoutCancel(ctx),
		entries: newSessionEntries(), fastPersistedEntries: map[string]fastPersistedEntry{},
		inflightSessions: map[string]*sessionFlight{}, rowsPending: map[string]bool{},
		claimedLegacySlackKeys: map[string]bool{}, transcriptReroutes: map[string]string{},
		hasActiveProcessesFn: hasActiveProcessesFn,
		writeSessionsJSON:    config == nil || config.WriteSessionsJSON, profileHomeCache: map[string]string{},
		sessionOwnerHints: map[string]string{}, dbHandleCache: NewRecoverableHandleCache(),
		spooledDropSessions: map[string]bool{}, dirtyTranscripts: map[string]*transcriptQueue{},
		transcriptAppendFailures: map[string]int{}}
	s.routingHome = home.Dir(ctx)
	_, _ = s.openSessionDBForActiveScope(ctx, "")
	return s
}

// Pins the session database for every later read, nil selecting the JSONL fallback
func (s *SessionStore) PinDB(db SessionStoreDB) {
	s.dbPinned, s.pinnedDB = true, db
}

// Whether the session has active work, failing closed when the registry errors
func (s *SessionStore) hasActiveProcessesSafe(sessionKey, context string) bool {
	if s.hasActiveProcessesFn == nil {
		return false
	}
	active, err := s.hasActiveProcessesFn(sessionKey)
	if err != nil {
		slog.Warn(fmt.Sprintf("has_active_processes_fn raised during %s for %s; keeping session alive: %v", context, sessionKey, err))
		return true
	}
	return active
}

// Whether any session was ever created, state.db counting ended sessions
func (s *SessionStore) HasAnySessions(ctx context.Context) bool {
	if db := s.db(ctx); db != nil {
		if ok, err := db.SessionCountGE(ctx, 2); err == nil {
			return ok
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	return s.entries.len() > 1
}

// Single flight lookup or create per routing key, overlapping callers sharing the owner's result
func (s *SessionStore) GetOrCreateSession(ctx context.Context, source *SessionSource, forceNew, touchActivity bool) (*SessionEntry, error) {
	if detachedRouteOf(source) != nil {
		return detachedEntry(ctx, s, source)
	}
	sessionKey, err := s.generateSessionKey(ctx, source, nil)
	if err != nil {
		return nil, err
	}
	s.inflightMu.Lock()
	slot, joined := s.inflightSessions[sessionKey]
	if !joined {
		slot = &sessionFlight{done: make(chan struct{})}
		s.inflightSessions[sessionKey] = slot
	}
	s.inflightMu.Unlock()
	if joined {
		<-slot.done
		if slot.err != nil {
			return nil, slot.err
		}
		if touchActivity {
			if err := s.UpdateSession(ctx, slot.result.SessionKey, nil, true); err != nil {
				return nil, err
			}
		}
		return slot.result, nil
	}
	defer func() {
		close(slot.done)
		s.inflightMu.Lock()
		delete(s.inflightSessions, sessionKey)
		s.inflightMu.Unlock()
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				slot.err = fmt.Errorf("%v", r)
				panic(r)
			}
		}()
		slot.result, slot.err = s.getOrCreateSessionImpl(ctx, source, forceNew, touchActivity)
	}()
	return slot.result, slot.err
}

// One routing transition for the single flight owner, blocking I/O outside the entries lock
func (s *SessionStore) getOrCreateSessionImpl(ctx context.Context, source *SessionSource, forceNew, touchActivity bool) (*SessionEntry, error) {
	sessionKey, err := s.generateSessionKey(ctx, source, nil)
	if err != nil {
		return nil, err
	}
	now := sessionNow()
	if !forceNew {
		if err := s.adoptLegacySlackEntry(ctx, source, sessionKey); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.ensureLoadedLocked(ctx)
	observed := s.entries.get(sessionKey)
	s.mu.Unlock()
	var checks *routeChecks
	if !forceNew && observed != nil {
		sid := observed.SessionID
		checks = &routeChecks{sessionID: sid, canonicalID: s.compressionTipForSessionID(ctx, sid),
			isStale:     s.isSessionEndedInDB(ctx, sid, "") && s.localReceiptTarget(ctx, observed) == "" && !s.apiBound(ctx, observed),
			resetReason: s.routeResetReason(observed)}
	}
	decision := s.applyRouteChecks(ctx, sessionKey, checks, forceNew, touchActivity, now, source)
	if decision.needsRecover && decision.prevSessionID == nil {
		if err := s.routeRecover(ctx, decision, sessionKey, source, now); err != nil {
			return nil, err
		}
	}
	var create *sessionCreateArgs
	if decision.entry == nil {
		create = s.routeCreate(decision, sessionKey, source, now, forceNew, observed)
	}
	err = func() error {
		if create != nil {
			defer s.rowSettled(create.SessionID)
		}
		if decision.needsSave {
			var err error
			if decision.metadataOnlySave {
				err = s.saveEntry(ctx, sessionKey, nil, false)
			} else {
				err = s.saveEntries(ctx)
			}
			if err != nil {
				return err
			}
		}
		endReason := "session_reset"
		if decision.resetReason != nil {
			endReason = *decision.resetReason
		}
		s.finishRouteTransition(ctx, sessionKey, decision.prevSessionID, endReason, create, source, decision.entry.DisplayName, "")
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if observed == nil || decision.entry.SessionID != observed.SessionID {
		s.announceRoute(sessionKey, decision.entry)
	}
	return decision.entry, nil
}

// Applies the stale and reset decisions under the lock, a replaced entry reading as healthy
func (s *SessionStore) applyRouteChecks(ctx context.Context, sessionKey string, checks *routeChecks, forceNew, touchActivity bool, now pyDatetime, source *SessionSource) *routeDecision {
	decision := &routeDecision{}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	if forceNew {
		return decision
	}
	entry := s.entries.get(sessionKey)
	if entry == nil {
		decision.needsRecover = true
		return decision
	}
	snapshotSID, canonical := "", ""
	if checks != nil {
		snapshotSID, canonical = checks.sessionID, checks.canonicalID
	}
	healed := s.healCompressionTipLocked(entry, snapshotSID, canonical)
	checked := checks != nil && entry.SessionID == snapshotSID
	staleHit := checked && checks.isStale
	resetReason := ""
	if checked {
		resetReason = checks.resetReason
	}
	if staleHit {
		slog.Warn(fmt.Sprintf("gateway.session: routing key %q -> %s is ended in state.db but still live in sessions.json; "+
			"dropping stale entry and recovering/recreating the session (#54878)", sessionKey, entry.SessionID))
	}
	if staleHit || resetReason != "" {
		if resetReason != "" {
			decision.scheduleReset(resetReason, entry, entry.LastPromptTokens > 0)
		}
		s.entries.delete(sessionKey)
		decision.needsRecover = true
		return decision
	}
	if touchActivity {
		entry.UpdatedAt = now
		if entry.Origin == nil {
			entry.Origin = source
		}
	}
	decision.entry = entry
	decision.needsSave = touchActivity || healed
	decision.metadataOnlySave = touchActivity && !healed
	if touchActivity && entry.TransportProfile == nil {
		entry.TransportProfile = optionalString(TransportProfileOf(source))
	}
	return decision
}

// Pointer to a non empty string, nil for empty
func optionalString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// Adopts a recoverable state.db row for the vacant key
func (s *SessionStore) routeRecover(ctx context.Context, decision *routeDecision, sessionKey string, source *SessionSource, now pyDatetime) error {
	recovered, err := s.queryRecoverableSession(ctx, sessionKey, source, now)
	if err != nil {
		return err
	}
	if recovered == nil {
		return nil
	}
	s.reopenSessionRow(ctx, sessionKey, recovered.SessionID, "")
	s.mu.Lock()
	if current := s.entries.get(sessionKey); current != nil {
		decision.entry = current
	} else {
		s.entries.set(sessionKey, recovered)
		decision.entry = recovered
	}
	s.mu.Unlock()
	decision.needsSave = true
	return nil
}

// Publishes a fresh candidate when the key is still vacant, returning its create arguments when it won
func (s *SessionStore) routeCreate(decision *routeDecision, sessionKey string, source *SessionSource, now pyDatetime, forceNew bool, observed *SessionEntry) *sessionCreateArgs {
	sessionID := newRoutedSessionID(now)
	candidate := NewSessionEntry(sessionKey, sessionID, now, now)
	candidate.Origin, candidate.DisplayName, candidate.Platform, candidate.ChatType = source, source.ChatName, source.Platform, source.ChatType
	candidate.WasAutoReset, candidate.AutoResetReason = decision.resetReason != nil, decision.resetReason
	candidate.ResetHadActivity, candidate.PrevSessionID = decision.resetHadActivity, decision.prevSessionID
	candidate.TransportProfile = optionalString(TransportProfileOf(source))
	prev := ""
	if decision.prevSessionID != nil {
		prev = *decision.prevSessionID
	}
	create := sessionCreateKwargs(sessionID, sessionKey, source, string(source.Platform), source.ChatName, prev)
	s.mu.Lock()
	current := s.entries.get(sessionKey)
	if current == nil || (forceNew && current == observed) {
		s.entries.set(sessionKey, candidate)
		current = candidate
		s.rowsPending[sessionID] = true
	}
	s.mu.Unlock()
	decision.entry, decision.needsSave = current, true
	if current == candidate {
		return create
	}
	return nil
}

// Updates lightweight metadata after an interaction, internal turns leaving the activity clock alone
func (s *SessionStore) UpdateSession(ctx context.Context, sessionKey string, lastPromptTokens *int64, touchActivity bool) error {
	s.mu.Lock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil {
		s.mu.Unlock()
		return nil
	}
	if touchActivity {
		entry.UpdatedAt = sessionNow()
	}
	if lastPromptTokens != nil {
		entry.LastPromptTokens = *lastPromptTokens
	}
	sid, origin, name, transport := entry.SessionID, entry.Origin, entry.DisplayName, entry.TransportProfile
	s.mu.Unlock()
	if err := s.saveEntry(ctx, sessionKey, nil, false); err != nil {
		return err
	}
	return s.recordGatewaySessionPeer(ctx, sid, sessionKey, origin, name, false, transport)
}

// Metadata value stored on a live entry, the default when absent
func (s *SessionStore) GetSessionMetadata(ctx context.Context, sessionKey, key string, def any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || entry.Metadata == nil {
		return def
	}
	if v, ok := entry.Metadata.Get(key); ok {
		return v
	}
	return def
}

// Persists a small JSON value on the entry without advancing its activity clock
func (s *SessionStore) SetSessionMetadata(ctx context.Context, sessionKey, key string, value any) (bool, error) {
	return s.updateEntry(ctx, sessionKey, func(e *SessionEntry) bool {
		if e.Metadata == nil {
			e.Metadata = pyjson.NewObject()
		}
		e.Metadata.Set(key, value)
		return true
	})
}

// Persists or clears the model override with its non secret keys only, durable before published
func (s *SessionStore) SetModelOverride(ctx context.Context, sessionKey string, override any) error {
	cleaned := SanitizeModelOverride(override)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || modelOverrideEqual(entry.ModelOverride, cleaned) {
		return nil
	}
	data, generation := s.snapshotRoutingLocked(ctx)
	entry = s.entries.get(sessionKey)
	candidate := *entry
	candidate.ModelOverride = cleaned
	data.Set(sessionKey, candidate.ToDict())
	if err := s.persistRoutingData(ctx, data, generation); err != nil {
		return err
	}
	entry.ModelOverride = cleaned
	return nil
}

// Python equality of two optional overrides
func modelOverrideEqual(a, b *pyjson.Object) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return pyjson.Equal(a, b)
}

// Persists the lane yolo toggle, false when the key has no entry or it is unchanged
func (s *SessionStore) SetSessionYolo(ctx context.Context, sessionKey string, enabled *bool) (bool, error) {
	return s.updateEntry(ctx, sessionKey, func(e *SessionEntry) bool {
		if (e.Yolo == nil && enabled == nil) || (e.Yolo != nil && enabled != nil && *e.Yolo == *enabled) {
			return false
		}
		if enabled == nil {
			e.Yolo = nil
		} else {
			v := *enabled
			e.Yolo = &v
		}
		return true
	})
}

// Copy of the persisted model override, nil when none
func (s *SessionStore) GetModelOverride(ctx context.Context, sessionKey string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || entry.ModelOverride == nil || entry.ModelOverride.Len() == 0 {
		return nil
	}
	return entry.ModelOverride.Copy()
}

// Forces a reset onto a fresh session id, local routes through the authority reset
func (s *SessionStore) ResetSession(ctx context.Context, sessionKey string, displayName *string) (*SessionEntry, error) {
	s.mu.Lock()
	old := s.entryLocked(ctx, sessionKey)
	if old == nil {
		s.mu.Unlock()
		return nil, nil
	}
	now := sessionNow()
	sessionID := newRoutedSessionID(now)
	if old.Origin != nil && old.Origin.Platform == PlatformLocal {
		defer s.mu.Unlock()
		return resetLocalSession(ctx, s, old, sessionID, now, displayName)
	}
	sourceValue := "unknown"
	if old.Platform != "" {
		sourceValue = string(old.Platform)
	}
	create := sessionCreateKwargs(sessionID, sessionKey, old.Origin, sourceValue, old.DisplayName, old.SessionID)
	name := old.DisplayName
	if displayName != nil {
		name = displayName
	}
	fresh, err := s.replaceRouteLocked(ctx, sessionKey, old, sessionID, now, func(e *SessionEntry) {
		e.DisplayName, e.IsFreshReset = name, true
	})
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	func() {
		defer s.rowSettled(sessionID)
		endID := old.SessionID
		s.finishRouteTransition(ctx, sessionKey, &endID, "session_reset", create, old.Origin, fresh.DisplayName, " during reset")
	}()
	s.announceRoute(sessionKey, fresh)
	return fresh, nil
}

// Publishes a fresh entry inheriting origin, platform, chat type and transport profile, then saves, lock held
func (s *SessionStore) replaceRouteLocked(ctx context.Context, sessionKey string, old *SessionEntry, sessionID string, now pyDatetime, fields func(*SessionEntry)) (*SessionEntry, error) {
	fresh := NewSessionEntry(sessionKey, sessionID, now, now)
	fresh.Origin, fresh.Platform, fresh.ChatType, fresh.TransportProfile = old.Origin, old.Platform, old.ChatType, old.TransportProfile
	fields(fresh)
	s.entries.set(sessionKey, fresh)
	if err := s.save(ctx); err != nil {
		return nil, err
	}
	s.rowsPending[sessionID] = true
	return fresh, nil
}

// Rekeys the live routing index from one profile namespace to another refusing collisions
func (s *SessionStore) RekeyProfileRouting(ctx context.Context, oldName, newName string) (int, error) {
	old, fresh := strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if old == "" || fresh == "" || old == fresh {
		return 0, nil
	}
	oldNS, newNS := "agent:"+old+":", "agent:"+fresh+":"
	s.mu.Lock()
	defer s.mu.Unlock()
	var moving []string
	for _, key := range s.entries.keyList() {
		if strings.HasPrefix(key, oldNS) {
			moving = append(moving, key)
		}
	}
	for _, key := range moving {
		if target := newNS + key[len(oldNS):]; s.entries.has(target) {
			return 0, &SessionEntryError{Kind: "ValueError", Msg: fmt.Sprintf("profile routing collision while renaming %s to %s: %s already exists",
				pyReprString(old), pyReprString(fresh), pyReprString(target))}
		}
	}
	for _, key := range moving {
		newKey := newNS + key[len(oldNS):]
		entry := s.entries.delete(key)
		moved := *entry
		if origin := entry.Origin; origin != nil && origin.Profile != nil && *origin.Profile == old {
			moved.Origin = ReplaceSource(origin, func(src *SessionSource) { src.Profile = &fresh })
		}
		moved.SessionKey = newKey
		s.entries.set(newKey, &moved)
	}
	if len(moving) > 0 {
		if err := s.save(ctx); err != nil {
			return len(moving), err
		}
	}
	return len(moving), nil
}

// Drops a deleted profile's live routing entries and persists the drop
func (s *SessionStore) PurgeProfileRouting(ctx context.Context, profile string) (int, error) {
	name := strings.TrimSpace(profile)
	if name == "" {
		return 0, nil
	}
	ns := "agent:" + name + ":"
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []string
	for _, key := range s.entries.keyList() {
		if strings.HasPrefix(key, ns) {
			dropped = append(dropped, key)
		}
	}
	for _, key := range dropped {
		s.entries.delete(key)
	}
	if len(dropped) > 0 {
		if err := s.save(ctx); err != nil {
			return len(dropped), err
		}
	}
	return len(dropped), nil
}

// Drops every routing entry naming the hard deleted session and persists the drop
func (s *SessionStore) RemoveBySessionID(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	s.mu.Lock()
	s.ensureLoadedLocked(ctx)
	var dropped []string
	for _, e := range s.entries.values() {
		if e.SessionID == sessionID {
			dropped = append(dropped, e.SessionKey)
		}
	}
	for _, key := range dropped {
		s.entries.delete(key)
	}
	var err error
	if len(dropped) > 0 {
		delete(s.sessionOwnerHints, sessionID)
		err = s.save(ctx)
	}
	s.mu.Unlock()
	if len(dropped) > 0 {
		plural := "ies"
		if len(dropped) == 1 {
			plural = "y"
		}
		slog.Info(fmt.Sprintf("SessionStore removed %d routing entr%s for deleted session %s", len(dropped), plural, sessionID))
	}
	return len(dropped), err
}

// Points the key at an existing session, ending the current row and reopening the target
func (s *SessionStore) SwitchSession(ctx context.Context, sessionKey, targetSessionID string, expectedSessionID *string, preservePromptPin bool) (*SessionEntry, error) {
	s.mu.Lock()
	old := s.entryLocked(ctx, sessionKey)
	if old == nil {
		s.mu.Unlock()
		return nil, nil
	}
	if expectedSessionID != nil && old.SessionID != *expectedSessionID {
		slog.Info(fmt.Sprintf("Session switch for %s refused: route moved from %s to %s after the caller's snapshot", sessionKey, *expectedSessionID, old.SessionID))
		s.mu.Unlock()
		return nil, nil
	}
	if old.SessionID == targetSessionID {
		s.mu.Unlock()
		return old, nil
	}
	fresh, err := s.replaceRouteLocked(ctx, sessionKey, old, targetSessionID, sessionNow(), func(e *SessionEntry) {
		e.DisplayName, e.ModelOverride, e.Yolo = old.DisplayName, old.ModelOverride, old.Yolo
		if preservePromptPin && old.PromptPin != nil {
			e.PromptPin = old.PromptPin.Copy()
		}
	})
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	err = func() error {
		defer s.rowSettled(targetSessionID)
		if s.dbForKey(ctx, sessionKey) != nil && old.SessionID != "" {
			s.promoteSessionReset(ctx, sessionKey, old.SessionID, "session_switch", func(err error) {
				slog.Debug(fmt.Sprintf("Session DB end_session failed: %v", err))
			})
		}
		if s.dbForKey(ctx, sessionKey) != nil {
			s.reopenSessionRow(ctx, sessionKey, targetSessionID, "Session DB reopen_session failed")
			return s.recordGatewaySessionPeer(ctx, targetSessionID, sessionKey, fresh.Origin, fresh.DisplayName, true, fresh.TransportProfile)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	s.announceRoute(sessionKey, fresh)
	return fresh, nil
}

// Every routing entry newest first, only those active within the minutes when given
func (s *SessionStore) ListSessions(ctx context.Context, activeMinutes *int) []*SessionEntry {
	s.mu.Lock()
	s.ensureLoadedLocked(ctx)
	entries := s.entries.values()
	s.mu.Unlock()
	if activeMinutes != nil {
		cutoff := sessionNow().T.Add(-time.Duration(*activeMinutes) * time.Minute)
		cut := float64(cutoff.UnixNano()) / 1e9
		entries = slices.DeleteFunc(entries, func(e *SessionEntry) bool { return e.UpdatedAt.timestamp() < cut })
	}
	slices.SortStableFunc(entries, func(a, b *SessionEntry) int {
		ta, tb := a.UpdatedAt.timestamp(), b.UpdatedAt.timestamp()
		switch {
		case ta > tb:
			return -1
		case ta < tb:
			return 1
		}
		return 0
	})
	return entries
}

// Live entry routing to the persisted session id, nil when none
func (s *SessionStore) LookupBySessionID(ctx context.Context, sessionID string) *SessionEntry {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	for _, e := range s.entries.values() {
		if e.SessionID == sessionID {
			return e
		}
	}
	return nil
}

// Persisted routing entry for the exact key, nil when none
func (s *SessionStore) LookupBySessionKey(ctx context.Context, sessionKey string) *SessionEntry {
	if sessionKey == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryLocked(ctx, sessionKey)
}

// Session id the key routes to under the lock, false when unknown
func (s *SessionStore) PeekSessionID(sessionKey string) (string, bool) {
	if sessionKey == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(s.ctx, sessionKey)
	if entry == nil {
		return "", false
	}
	return entry.SessionID, true
}
