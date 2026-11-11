package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	rowReasoningKeys      = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}
	sessionFilenameUnsafe = regexp.MustCompile(`[^\p{L}\p{N}_-]`)
)

// Agent slots the session db flush reads and writes across flushes
type sessionPersistenceState struct {
	flushedDBMessageSessionID        *string
	flushedDBMessageIDs              map[any]bool
	dbStoreUIDScopeSession           *string
	dbStoreUIDScope                  map[string]bool
	muteNotificationReply            bool
	sessionRowReplayPending          *string
	activeCompressionLockHolder      *string
	activeSessionTurnLeaseHolder     *string
	activeSessionTurnLeaseTTLSeconds float64
}

// Session db writes the flush makes, swapped by tests that hold a writer inside its batch
var (
	sessionDBAppendBatch      = SessionStore.AppendMessagesBatch
	sessionDBFlushTokenCounts = SessionStore.FlushTokenCountsChecked
)

func sessionPersistenceLog() *slog.Logger { return slog.Default().With("logger", "run_agent") }

// Identity of a row for the flush, the map header for dicts and the pointer for live messages
func rowIdentity(v any) any {
	if m, ok := v.(map[string]any); ok {
		if m == nil {
			return nil
		}
		return reflect.ValueOf(m).Pointer()
	}
	if m, ok := v.(*state.Message); ok && m != nil {
		return m
	}
	return nil
}

// Reports internal recovery scaffolding that must never reach the durable transcript
func isEphemeralScaffolding(msg any) bool { return state.IsEphemeralScaffolding(msg) }

// Path safe component for a session id, hashed when sanitizing changed it so distinct ids never collide
func safeSessionFilenameComponent(sessionID any) string {
	raw := ""
	if pyTruthy(sessionID) {
		raw = strings.TrimSpace(pyrepr.Str(sessionID))
	}
	sanitized := []rune(strings.Trim(sessionFilenameUnsafe.ReplaceAllString(raw, "_"), "._"))
	if len(sanitized) > 96 {
		sanitized = sanitized[:96]
	}
	out := string(sanitized)
	if out == "" {
		out = "session"
	}
	if raw != "" && out == raw {
		return out
	}
	sum := sha256.Sum256([]byte(raw))
	return out + "_" + hex.EncodeToString(sum[:])[:12]
}

// Whether the persist override may replace content, never native media under a text override nor a merged summary
func overrideReplacesContent(msg map[string]any, content, override any) bool {
	if override == nil || pyTruthy(msg[state.CompressedSummaryMetadataKey]) {
		return false
	}
	_, contentList := content.([]any)
	_, overrideList := override.([]any)
	return !contentList || overrideList
}

// Replaces only the absorbed current turn, keeping the merged unanswered prefix in front of the override
func contentWithTurnOverride(msg map[string]any, content, override any) any {
	prefix, okPrefix := msg[state.MergedTurnPrefix].(string)
	text, okContent := content.(string)
	over, okOverride := override.(string)
	if okPrefix && okContent && okOverride && (text == prefix || strings.HasPrefix(text, prefix+"\n\n")) {
		if over != "" {
			return prefix + "\n\n" + over
		}
		return prefix
	}
	return override
}

// Content and api_content of the current turn user row as written, the live bytes becoming the sidecar
func durableUserRowContentDict(a *AIAgent, msg map[string]any, content any, apiContent *string) (any, *string) {
	override := a.persistUserMessageOverride
	if overrideReplacesContent(msg, content, override) {
		if s, ok := content.(string); apiContent == nil && ok && !reflect.DeepEqual(content, override) {
			apiContent = &s
		}
		content = contentWithTurnOverride(msg, content, override)
	}
	return content, apiContent
}

// Live message form of the durable user row content shared by the flush and the turn start stamp
func durableUserRowContent(a *AIAgent, msg *state.Message, content any, apiContent *string) (any, *string) {
	return durableUserRowContentDict(a, state.MessageToDict(msg), content, apiContent)
}

// Hides standalone compaction handoffs while merge into tail carriers keep their visibility
func summaryDisplayKind(msg map[string]any) any {
	if pyTruthy(msg[state.CompressedSummaryMetadataKey]) && UserOriginatedTurnView(msg) == nil &&
		(state.ClassifySummaryContent(msg["content"]) == "standalone" || !pyTruthy(msg[state.CompressedSummaryHasUserTurnKey])) {
		return "hidden"
	}
	return msg["display_kind"]
}

// Text only db projection, multimodal envelopes as their summary and images as screenshot markers
func durableContent(content any) any { return state.DurableContent(content) }

// Takes the reentrant persistence lock and returns its release, a no op for agents built without one
func persistLock(a *AIAgent) func() {
	lock := a.sessionPersistLock
	if lock == nil {
		return func() {}
	}
	lock.Lock()
	return lock.Unlock
}

// Stages the CLI user row of the next turn under the persist lock, clearing the previous turn overrides first
func (a *AIAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	defer persistLock(a)()
	a.persistUserMessageIdx = nil
	a.persistUserMessageOverride = nil
	a.persistUserMessageTimestamp = nil
	a.pendingCLIUserMessage = staged
	if appendToTranscript != nil {
		appendToTranscript()
	}
}

// Staged CLI user row awaiting its turn, nil once the turn adopted or dropped it
func (a *AIAgent) PendingCLIUserMessage() map[string]any {
	defer persistLock(a)()
	staged, _ := a.pendingCLIUserMessage.(map[string]any)
	return staged
}

// Persist index, clean message override and timestamp override the running turn staged, each nil when unset
func (a *AIAgent) PersistUserMessageStaging() (idx *int, override, timestamp any) {
	defer persistLock(a)()
	return a.persistUserMessageIdx, a.persistUserMessageOverride, a.persistUserMessageTimestamp
}

// Flushes the live transcript plus an accepted but unflushed CLI input before the session ends, true when it ran
func (a *AIAgent) PersistActiveSessionBeforeClose(ctx context.Context, readCLIHistory func() []map[string]any) bool {
	defer persistLock(a)()
	cliHistory := readCLIHistory()
	live := a.sessionMessages
	var rows []map[string]any
	var keys []any
	if live == nil {
		for _, row := range cliHistory {
			rows, keys = append(rows, row), append(keys, rowIdentity(row))
		}
	} else {
		for _, m := range live {
			if m != nil {
				rows, keys = append(rows, state.MessageToDict(m)), append(keys, m)
			}
		}
	}
	pending, _ := a.pendingCLIUserMessage.(map[string]any)
	pendingKey := rowIdentity(pending)
	staged := pending != nil && !pyTruthy(pending[state.DBPersistedMarker]) && !slices.Contains(keys, pendingKey)
	if staged {
		rows, keys = append(rows, pending), append(keys, pendingKey)
	}
	if len(rows) == 0 {
		return false
	}
	history := cliHistory
	switch n := len(history); {
	case n > 0 && pending != nil && rowIdentity(history[n-1]) == pendingKey:
		history = history[:n-1]
	case live == nil:
		history = nil
	}
	if a.cachedSystemPrompt == "" {
		if err := restoreOrBuildSystemPrompt(ctx, a, nil, history); err != nil {
			sessionPersistenceLog().Debug("Could not build system prompt during CLI close", "error", err)
			return false
		}
	}
	if a.cachedSystemPrompt == "" {
		return false
	}
	if err := a.ensureDBSession(); err != nil {
		sessionPersistenceLog().Debug(fmt.Sprintf("Could not persist active CLI session before close: %s", err))
		return false
	}
	for len(rows) > 0 {
		last := rows[len(rows)-1]
		if !(pyTruthy(last["_empty_recovery_synthetic"]) || pyTruthy(last["_empty_terminal_sentinel"])) {
			break
		}
		rows, keys = rows[:len(rows)-1], keys[:len(keys)-1]
	}
	historyKeys := map[any]bool{}
	for _, row := range history {
		if key := rowIdentity(row); key != nil {
			historyKeys[key] = true
		}
	}
	if _, err := a.flushRowsUnlocked(rows, keys, historyKeys, 1); err != nil {
		sessionPersistenceLog().Debug("session flush failed", "error", err)
	}
	persisted := make([]*state.Message, len(rows))
	for i, row := range rows {
		if m, isLive := keys[i].(*state.Message); isLive {
			*m = *state.MessageFromDict(row)
			persisted[i] = m
			continue
		}
		persisted[i] = state.MessageFromDict(row)
	}
	a.sessionMessages = persisted
	if a.sessionDB != nil {
		if _, err := sessionDBFlushTokenCounts(a.sessionDB, 5.0); err != nil {
			sessionPersistenceLog().Debug(fmt.Sprintf("Could not persist active CLI session before close: %s", err))
			return false
		}
	}
	noteTurnPersisted(a)
	return true
}

// Restages the unanswered tail user row as this turn's message when only failed tool scaffolding follows it
func adoptUnansweredTurn(history *[]map[string]any, query any, a *AIAgent) bool {
	rows := *history
	idx := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] != nil && rows[i]["role"] == "user" {
			idx = i
			break
		}
	}
	if idx < 0 || !reflect.DeepEqual(rows[idx]["content"], query) {
		return false
	}
	for _, row := range rows[idx+1:] {
		if row == nil || !(row["role"] == "tool" || (row["role"] == "assistant" && pyTruthy(row["tool_calls"]))) {
			return false
		}
	}
	tail := rows[idx]
	*history = rows[:idx]
	tail[state.DBPersistedMarker] = true
	a.pendingCLIUserMessage = tail
	return true
}

// One shot seed of already durable row identities, honoured only in the same session after a non empty flush
func dbFlushSeedIDs(a *AIAgent) map[any]bool {
	current := a.sessionID
	sameSession := a.flushedDBMessageSessionID != nil && *a.flushedDBMessageSessionID == current
	var seed map[any]bool
	if sameSession && a.lastFlushedDBIdx != 0 {
		seed = a.flushedDBMessageIDs
	}
	a.flushedDBMessageSessionID = &current
	if seed == nil {
		return map[any]bool{}
	}
	return seed
}

// Message uids this agent saw durable in its current session, the provenance for the reinsert guard
func dbFlushStoreUIDs(a *AIAgent) map[string]bool {
	if a.dbStoreUIDScope == nil || a.dbStoreUIDScopeSession == nil || *a.dbStoreUIDScopeSession != a.sessionID {
		sessionID := a.sessionID
		a.dbStoreUIDScopeSession, a.dbStoreUIDScope = &sessionID, map[string]bool{}
	}
	return a.dbStoreUIDScope
}

// Length of the identity matched and still marked prefix of the previous flush snapshot
func dbFlushScanStart(a *AIAgent, rows []map[string]any, keys []any) int {
	prefix, _ := a.dbFlushScanPrefix.([]any)
	scanStart := 0
	for i := 0; i < len(prefix) && i < len(rows); i++ {
		if keys[i] == nil || keys[i] != prefix[i] || rows[i] == nil || !pyTruthy(rows[i][state.DBPersistedMarker]) {
			break
		}
		scanStart++
	}
	return scanStart
}

// Session db row for a message with the persist override applied to the current turn user row only
func dbFlushRow(a *AIAgent, msg map[string]any, isCurrentTurnUser bool) map[string]any {
	role := msg["role"]
	if _, has := msg["role"]; !has {
		role = "unknown"
	}
	content := msg["content"]
	var apiContent *string
	if s, ok := msg["api_content"].(string); ok {
		apiContent = &s
	}
	timestamp := msg["timestamp"]
	if isCurrentTurnUser && role == "user" {
		content, apiContent = durableUserRowContentDict(a, msg, content, apiContent)
		if a.persistUserMessageTimestamp != nil {
			timestamp = a.persistUserMessageTimestamp
		}
	}
	if apiContent != nil && reflect.DeepEqual(any(*apiContent), content) {
		apiContent = nil
	}
	if s, ok := content.(string); apiContent == nil && (role == "user" || role == "assistant") && ok && s != "" &&
		strings.TrimSpace(state.SanitizeContext(s)) != strings.TrimSpace(s) {
		apiContent = &s
	}
	toolName := msg["tool_name"]
	if !pyTruthy(toolName) {
		toolName = nil
		if role == "tool" {
			toolName = msg["name"]
		}
	}
	var toolCalls any
	if tc, ok := msg["tool_calls"].([]any); ok {
		toolCalls = tc
	}
	platformMessageID := msg["platform_message_id"]
	if !pyTruthy(platformMessageID) {
		platformMessageID = msg["message_id"]
	}
	row := map[string]any{
		"role": role, "content": durableContent(content), "tool_name": toolName, "tool_calls": toolCalls,
		"tool_call_id": msg["tool_call_id"], "effect_disposition": msg["effect_disposition"],
		"token_count": msg["token_count"], "finish_reason": msg["finish_reason"],
	}
	for _, k := range rowReasoningKeys {
		row[k] = msg[k]
	}
	row["_compressed_summary"] = pyTruthy(msg[state.CompressedSummaryMetadataKey])
	row["timestamp"] = timestamp
	row["api_content"] = nil
	if apiContent != nil {
		row["api_content"] = *apiContent
	}
	row["display_kind"] = summaryDisplayKind(msg)
	row["display_metadata"] = msg["display_metadata"]
	row["platform_message_id"] = platformMessageID
	row["observed"] = pyTruthy(msg["observed"])
	switch id := msg["_row_id"].(type) {
	case int, int64:
		row["_row_id"] = id
	}
	state.CopyIdentityFields(msg, row)
	if snap, ok := msg[state.DBRowSnapshot].(string); ok {
		row[state.DBRowSnapshot] = snap
	}
	return row
}

// Carries an end of batch rewrite of already flushed tool results onto their durable rows, returning the count
func syncRewrittenToolRowsDicts(a *AIAgent, batch []map[string]any, contentsBefore []any) int {
	rewriter, ok := a.sessionDB.(prunedRowRewriter)
	if a.persistDisabled || a.sessionID == "" || !ok {
		return 0
	}
	var changedMsgs []map[string]any
	var changes []state.PruneChange
	for i, msg := range batch {
		if i >= len(contentsBefore) || msg == nil {
			continue
		}
		if pyTruthy(msg[state.DBPersistedMarker]) && msg["role"] == "tool" && !reflect.DeepEqual(msg["content"], contentsBefore[i]) {
			before := make(map[string]any, len(msg))
			for k, v := range msg {
				before[k] = v
			}
			before["content"] = contentsBefore[i]
			changedMsgs = append(changedMsgs, msg)
			changes = append(changes, state.PruneChange{Original: dbFlushRow(a, before, false), Replacement: dbFlushRow(a, msg, false)})
		}
	}
	if len(changes) == 0 {
		return 0
	}
	ttl := a.activeSessionTurnLeaseTTLSeconds
	if ttl == 0 {
		ttl = 300.0
	}
	written, err := func() (int, error) {
		defer persistLock(a)()
		return rewriter.RewritePrunedRows(a.homeContext(), a.sessionID, changes, state.PruneRewriteOptions{
			TurnLeaseHolder: a.activeSessionTurnLeaseHolder, TurnLeaseTTLSeconds: &ttl,
		})
	}()
	if err != nil {
		var unresolved *state.PruneRowUnresolvedError
		var stale *state.PruneRowStaleError
		var leaseLost *state.SessionTurnLeaseLostError
		if errors.As(err, &unresolved) || errors.As(err, &stale) || errors.As(err, &leaseLost) {
			sessionPersistenceLog().Info(fmt.Sprintf("Tool-result rewrite not persisted; the stored row keeps the full body: %s", err))
			return 0
		}
		sessionPersistenceLog().Warn(fmt.Sprintf("Tool-result rewrite not persisted: %s", err))
		return 0
	}
	for i, msg := range changedMsgs {
		if snap, ok := changes[i].Replacement[state.DBRowSnapshot].(string); ok {
			msg[state.DBRowSnapshot] = snap
		}
	}
	return written
}

// Live message form of the tool row rewrite sync, carrying the refreshed snapshot back onto each message
func syncRewrittenToolRows(a *AIAgent, batch []*state.Message, contentsBefore []any) int {
	rows := messagesToRows(batch)
	written := syncRewrittenToolRowsDicts(a, rows, contentsBefore)
	for i, m := range batch {
		if m == nil || rows[i] == nil {
			continue
		}
		if snap, ok := rows[i][state.DBRowSnapshot].(string); ok {
			if m.Extra == nil {
				m.Extra = map[string]any{}
			}
			m.Extra[state.DBRowSnapshot] = snap
		}
	}
	return written
}

// SyncRewrittenToolRows carries the turn budget spill rewrite of flushed tool results onto their stored rows
func (a *AIAgent) SyncRewrittenToolRows(batch []*state.Message, contentsBefore []any) {
	syncRewrittenToolRows(a, batch, contentsBefore)
}

// Rows and messages still unflushed, stamping history and seeded rows durable unless the history replays
func dbFlushCollect(a *AIAgent, rows []map[string]any, keys []any, historyKeys map[any]bool, replayHistory bool) ([]map[string]any, []map[string]any) {
	seedIDs := dbFlushSeedIDs(a)
	pendingKey := rowIdentity(a.pendingCLIUserMessage)
	var batchRows, batchMsgs []map[string]any
	owners := map[uintptr]*state.ToolCallOwner{}
	storeUIDs := dbFlushStoreUIDs(a)
	for idx := dbFlushScanStart(a, rows, keys); idx < len(rows); idx++ {
		msg := rows[idx]
		if msg == nil || isEphemeralScaffolding(msg) {
			continue
		}
		uid, hasUID := state.MessageUIDOrNone(msg)
		if pyTruthy(msg[state.DBPersistedMarker]) {
			if hasUID {
				storeUIDs[uid] = true
			}
			continue
		}
		key := keys[idx]
		isHistory := key != nil && historyKeys[key]
		if ((isHistory && !replayHistory) || (key != nil && seedIDs[key])) && !pyTruthy(msg[state.PersistAfterAdmissionInterrupt]) {
			msg[state.DBPersistedMarker] = true
			if hasUID {
				storeUIDs[uid] = true
			}
			continue
		}
		if a.muteNotificationReply && !isHistory {
			msg["display_kind"] = "hidden"
			metadata := map[string]any{}
			if prior, ok := msg["display_metadata"].(map[string]any); ok {
				for k, v := range prior {
					metadata[k] = v
				}
			}
			metadata["notification_category"] = "diagnostic"
			msg["display_metadata"] = metadata
		}
		if msg["role"] == "tool" && !pyTruthy(msg[state.ToolCallUID]) {
			if toolUID, ok := state.ToolCallUIDFromHistory(rows, idx, owners); ok {
				msg[state.ToolCallUID] = toolUID
			}
		}
		currentTurn := (a.persistUserMessageIdx != nil && *a.persistUserMessageIdx == idx) || (key != nil && key == pendingKey)
		row := dbFlushRow(a, msg, currentTurn)
		if hasUID && storeUIDs[uid] {
			row[state.RestoredFromStore] = true
		}
		batchRows = append(batchRows, row)
		batchMsgs = append(batchMsgs, msg)
	}
	return batchRows, batchMsgs
}

// Writes the turn's new rows in one transaction, then stamps markers and mirrors the checkpoint drop
func dbFlushWrite(a *AIAgent, batchRows, batchMsgs, rows []map[string]any) error {
	if len(batchRows) == 0 {
		return nil
	}
	ttl := a.activeSessionTurnLeaseTTLSeconds
	if ttl == 0 {
		ttl = 300.0
	}
	_, err := sessionDBAppendBatch(a.sessionDB, a.homeContext(), a.sessionID, batchRows, state.AppendGuards{
		CompressionLockHolder: a.activeCompressionLockHolder, TurnLeaseHolder: a.activeSessionTurnLeaseHolder, TurnLeaseTTLSeconds: &ttl,
	}, nil)
	if err != nil {
		return err
	}
	state.SyncFlushedMessageMarkers(batchMsgs, batchRows)
	storeUIDs := dbFlushStoreUIDs(a)
	for _, msg := range batchMsgs {
		if uid, ok := state.MessageUIDOrNone(msg); ok {
			storeUIDs[uid] = true
		}
	}
	if newestCheckpointCarrier(batchMsgs, "codex_reasoning_items") >= 0 {
		DropShadowedCheckpoints(rows, "codex_reasoning_items", nil)
	}
	return nil
}

// Moves a compression closed session onto its live continuation tip, refusing a missing or ended tip
func dbFlushAdoptCompressionTip(a *AIAgent) bool {
	oldID := a.sessionID
	ctx := a.homeContext()
	tip, err := a.sessionDB.GetCompressionTip(ctx, oldID)
	if err != nil {
		sessionPersistenceLog().Warn(fmt.Sprintf("compression tip lookup failed for %s: %s", oldID, err))
		return false
	}
	if tip == "" || tip == oldID {
		return false
	}
	tipRow, err := a.sessionDB.GetSession(ctx, tip)
	if err != nil || tipRow == nil {
		return false
	}
	if ended, _ := tipRow.Get("ended_at"); ended != nil {
		return false
	}
	sessionPersistenceLog().Warn(fmt.Sprintf("Adopted live compression tip %s for closed session %s; retrying flush once", tip, oldID))
	a.sessionID, a.flushedDBMessageIDs, a.lastFlushedDBIdx = tip, map[any]bool{}, 0
	a.compressionAdoptionFailed = false
	return true
}

// Reports a session row confirmed absent, a failed lookup counting as present
func dbFlushSessionRowGone(a *AIAgent, sessionID string) bool {
	row, err := a.sessionDB.GetSession(a.homeContext(), sessionID)
	if err != nil {
		sessionPersistenceLog().Warn(fmt.Sprintf("session row lookup failed for %s: %s", sessionID, err))
		return false
	}
	return row == nil
}

// Classifies a failed flush and names its single retry, adopted or healed, or empty to fail closed
func dbFlushFailed(a *AIAgent, e error, batchRows []map[string]any, adoptionBudget int, rows []map[string]any) string {
	a.dbFlushScanPrefix = nil
	cause := state.ClassifyPersistenceError(e)
	a.lastPersistenceErrorCause = cause
	if cause == "session_row_missing" {
		if adoptionBudget <= 0 || !dbFlushSessionRowGone(a, a.sessionID) {
			return ""
		}
		stripPersistenceMarkers(rows)
		pending := a.sessionID
		a.sessionRowReplayPending = &pending
		a.flushedDBMessageIDs = map[any]bool{}
		a.lastFlushedDBIdx = 0
		a.sessionDBCreated = false
		_ = a.ensureDBSession()
		parentID := a.parentSessionID
		if !a.sessionDBCreated && parentID != "" && dbFlushSessionRowGone(a, parentID) {
			func() {
				a.parentSessionID = ""
				defer func() { a.parentSessionID = parentID }()
				_ = a.ensureDBSession()
			}()
		}
		if !a.sessionDBCreated {
			sessionPersistenceLog().Warn(fmt.Sprintf("Session DB row for %s is missing and could not be recreated; will retry next flush", orNone(a.sessionID)))
			return ""
		}
		sessionPersistenceLog().Warn(fmt.Sprintf("Session DB row for %s was removed under the live agent; recreated it and replaying the transcript", orNone(a.sessionID)))
		return "healed"
	}
	var replaced *state.StateDBReplacedError
	var corrupt *state.StateDBCorruptError
	if errors.As(e, &replaced) || errors.As(e, &corrupt) {
		diverted := make([]any, 0, len(batchRows))
		for _, r := range batchRows {
			clean := map[string]any{}
			for k, v := range r {
				if !state.RepairBookkeepingFields[k] {
					clean[k] = v
				}
			}
			diverted = append(diverted, clean)
		}
		if _, err := state.DivertSessionTranscriptJSONL(a.homeContext(), a.sessionID, diverted); err != nil {
			sessionPersistenceLog().Warn(fmt.Sprintf("JSONL divert failed after state.db %s for %s", cause, orNone(a.sessionID)), "error", err)
		}
	}
	var closed *state.CompressionSessionClosedError
	if errors.As(e, &closed) {
		if adoptionBudget > 0 && dbFlushAdoptCompressionTip(a) {
			return "adopted"
		}
		a.compressionAdoptionFailed = true
	}
	sessionPersistenceLog().Warn(fmt.Sprintf("Session DB append_message failed: %s", e))
	return ""
}

// Rewrites the current turn user dict in place with the persist override, timestamp and platform id
func (a *AIAgent) applyPersistUserMessageOverride(messages []map[string]any) {
	idx := a.persistUserMessageIdx
	override, timestamp, platformID := a.persistUserMessageOverride, a.persistUserMessageTimestamp, a.persistUserMessagePlatformID
	if idx == nil || (override == nil && timestamp == nil && platformID == nil) {
		return
	}
	if *idx < 0 || *idx >= len(messages) {
		return
	}
	msg := messages[*idx]
	if msg == nil || msg["role"] != "user" {
		return
	}
	if overrideReplacesContent(msg, msg["content"], override) {
		msg["content"] = contentWithTurnOverride(msg, msg["content"], override)
	}
	if timestamp != nil {
		msg["timestamp"] = timestamp
	}
	if platformID != nil {
		msg["platform_message_id"] = *platformID
	}
}

// Saves the transcript on any exit path after dropping trailing empty response scaffolding from the live list
func (a *AIAgent) persistSession(messages *[]*state.Message, conversationHistory []*state.Message) error {
	defer persistLock(a)()
	a.dropTrailingEmptyResponseScaffolding(messages)
	a.sessionMessages = *messages
	if _, err := a.flushLiveMessages(*messages, conversationHistory); err != nil {
		sessionPersistenceLog().Debug("session flush failed", "error", err)
	}
	if a.sessionDB != nil {
		if _, err := sessionDBFlushTokenCounts(a.sessionDB, 5.0); err != nil {
			return err
		}
	}
	noteTurnPersisted(a)
	return nil
}

// Writes the unpersisted transcript messages to the session store, resumed history identity skipped
func (a *AIAgent) PersistSession(messages *[]*state.Message, conversationHistory []*state.Message) error {
	return a.persistSession(messages, conversationHistory)
}

// Pops empty response retry scaffolding from the tail, keeping the tool rows already saved before it
func (a *AIAgent) dropTrailingEmptyResponseScaffolding(messages *[]*state.Message) {
	for len(*messages) > 0 {
		last := (*messages)[len(*messages)-1]
		if last == nil || !(pyTruthy(last.Extra["_empty_recovery_synthetic"]) || pyTruthy(last.Extra["_empty_terminal_sentinel"])) {
			return
		}
		*messages = (*messages)[:len(*messages)-1]
	}
}

// Flushes live messages through their dict views under the persist lock and writes every change back
func (a *AIAgent) flushLiveMessages(messages, conversationHistory []*state.Message) (bool, error) {
	defer persistLock(a)()
	rows := make([]map[string]any, len(messages))
	keys := make([]any, len(messages))
	for i, m := range messages {
		if m != nil {
			rows[i], keys[i] = state.MessageToDict(m), m
		}
	}
	historyKeys := map[any]bool{}
	for _, h := range conversationHistory {
		if h != nil {
			historyKeys[h] = true
		}
	}
	ok, err := a.flushRowsUnlocked(rows, keys, historyKeys, 1)
	for i, m := range messages {
		if m != nil && rows[i] != nil {
			*m = *state.MessageFromDict(rows[i])
		}
	}
	return ok, err
}

// Serialized flush of message dicts, written true, disabled false with no error, failed false with the error
func (a *AIAgent) flushMessagesToSessionDB(messages []map[string]any) (bool, error) {
	return a.flushMessagesToSessionDBWithHistory(messages, nil)
}

// Dict flush that stamps the identity matched conversation history rows as already durable
func (a *AIAgent) flushMessagesToSessionDBWithHistory(messages, conversationHistory []map[string]any) (bool, error) {
	defer persistLock(a)()
	keys := make([]any, len(messages))
	for i, m := range messages {
		keys[i] = rowIdentity(m)
	}
	historyKeys := map[any]bool{}
	for _, h := range conversationHistory {
		if k := rowIdentity(h); k != nil {
			historyKeys[k] = true
		}
	}
	return a.flushRowsUnlocked(messages, keys, historyKeys, 1)
}

// Persists unflushed rows deduplicated by the durable marker, retrying once after a tip adoption or a row heal
func (a *AIAgent) flushRowsUnlocked(rows []map[string]any, keys []any, historyKeys map[any]bool, adoptionBudget int) (bool, error) {
	if a.persistDisabled || a.sessionDB == nil {
		return false, nil
	}
	var batchRows []map[string]any
	err := func() error {
		if !a.sessionDBCreated {
			if err := a.ensureDBSession(); err != nil {
				return err
			}
		}
		replay := a.sessionRowReplayPending != nil && *a.sessionRowReplayPending == a.sessionID
		var batchMsgs []map[string]any
		batchRows, batchMsgs = dbFlushCollect(a, rows, keys, historyKeys, replay)
		return dbFlushWrite(a, batchRows, batchMsgs, rows)
	}()
	if err == nil {
		a.sessionRowReplayPending = nil
		a.flushedDBMessageIDs = map[any]bool{}
		a.lastFlushedDBIdx = len(rows)
		a.dbFlushScanPrefix = append([]any(nil), keys...)
		return true, nil
	}
	if dbFlushFailed(a, err, batchRows, adoptionBudget, rows) == "" {
		return false, err
	}
	return a.flushRowsUnlocked(rows, keys, historyKeys, 0)
}

// Messages before the last assistant turn, the rollback point for a malformed final answer, all when none
func (a *AIAgent) getMessagesUpToLastAssistant(messages []*state.Message) []*state.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i] != nil && messages[i].Role == "assistant" {
			return messages[:i]
		}
	}
	return append([]*state.Message(nil), messages...)
}

// Appends the conversation trajectory to its JSONL file when trajectories are saved
func (a *AIAgent) saveTrajectory(messages []map[string]any, userQuery string, completed bool) error {
	if !a.saveTrajectories {
		return nil
	}
	trajectory, err := a.convertToTrajectoryFormat(messages, userQuery)
	if err != nil {
		return err
	}
	SaveTrajectory(trajectory, a.model, completed, "")
	return nil
}

// AdoptUnansweredTurn restages the unanswered tail user row of a dispatcher re-run as this turn's message
func (a *AIAgent) AdoptUnansweredTurn(history *[]map[string]any, query any) bool {
	return adoptUnansweredTurn(history, query, a)
}
