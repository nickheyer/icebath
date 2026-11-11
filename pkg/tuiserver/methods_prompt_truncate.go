package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Params whose presence makes a submit a truncation
var truncationParams = []string{"truncate_before_user_ordinal", "truncate_before_row_id", "truncate_before_message_id"}

// Deep cuts only warn this release, the next one refuses them with 4033
var deepTruncateEnforced = false

// Resolved cut of a submit truncation, with the physical prefix when the target was absorbed into a live carrier
type truncationCut struct {
	ordinal       int
	cutIndex      int
	durablePrefix []map[string]any
	hasPrefix     bool
}

// Coerced row id and client ordinal of the truncation params, refusing malformed or unconfirmed ones
func parseTruncationParams(rid any, sid string, params map[string]any, history []map[string]any) (*int64, *int64, map[string]any) {
	var targetRowID, clientOrdinal *int64
	if raw := params["truncate_before_row_id"]; raw != nil {
		n, err := coerceTruncateInt(rid, raw, "truncate_before_row_id")
		if err != nil {
			return nil, nil, err
		}
		targetRowID = &n
	}
	if raw := params["truncate_before_user_ordinal"]; raw != nil {
		n, err := coerceTruncateInt(rid, raw, "truncate_before_user_ordinal")
		if err != nil {
			return nil, nil, err
		}
		clientOrdinal = &n
	}
	if provider.IsTruthyValue(params["confirm_truncate"]) {
		return targetRowID, clientOrdinal, nil
	}
	logger().Warn(fmt.Sprintf("prompt.submit: REFUSED unconfirmed truncation of session %s (%d messages held; "+
		"ordinal=%s, row_id=%s, message_id=%s). The client attached truncation parameters without "+
		"confirm_truncate — likely stale truncation parameters on an ordinary submit.",
		sid, len(history), optionalIntStr(clientOrdinal), optionalIntStr(targetRowID), pyrepr.Str(params["truncate_before_message_id"])))
	return nil, nil, rpcErr(rid, 4029, "truncation parameters require confirm_truncate=true; "+
		"an ordinary prompt.submit must not drop session history "+
		"(update your Hermes client if a rewind was intended)", nil)
}

// str of an optional integer, None when absent
func optionalIntStr(n *int64) string {
	if n == nil {
		return "None"
	}
	return strconv.FormatInt(*n, 10)
}

// Resolves the truncation target to its user ordinal and cut index, failing closed on a stale or ambiguous target
func resolveTruncationOrdinal(ctx context.Context, rid any, sid string, session *tuigateway.Session, params map[string]any, history []map[string]any) (truncationCut, map[string]any) {
	targetRowID, clientOrdinal, errResp := parseTruncationParams(rid, sid, params, history)
	if errResp != nil {
		return truncationCut{}, errResp
	}
	truncateMessageID := params["truncate_before_message_id"]
	var cut truncationCut
	prefixUserCount := len(historyUserIndices(historyAsMessages(historyList(session.Get("display_history_prefix")))))
	userIndices := historyUserIndices(history)
	stale := func(resolved *int64) (truncationCut, map[string]any) {
		var ordinal, segment any
		if clientOrdinal != nil {
			ordinal = *clientOrdinal
			segment = *clientOrdinal - int64(prefixUserCount)
		} else if resolved != nil {
			segment = *resolved
		}
		return truncationCut{}, rpcErr(rid, 4018, staleTargetMsg, map[string]any{
			"user_turn_count": len(userIndices), "ordinal": ordinal,
			"segment_ordinal": segment, "prefix_user_count": prefixUserCount})
	}
	var ordinal int64
	if targetRowID != nil || truncateMessageID != nil {
		var paramName, targetRepr, notFound string
		found := false
		if targetRowID != nil {
			paramName, targetRepr = "truncate_before_row_id", strconv.FormatInt(*targetRowID, 10)
			if match, ok := resolveTruncateRowID(ctx, session, history, *targetRowID); ok {
				found, ordinal = true, int64(match.ordinal)
				if match.hasPrefix {
					cut.durablePrefix, cut.hasPrefix = match.durablePrefix, true
				}
			}
			notFound = "target row_id %s not found for session %s (in-memory + durable)"
		} else {
			paramName = "truncate_before_message_id"
			targetRepr = pyrepr.Str(truncateMessageID)
			for userOrdinal, index := range userIndices {
				if pyjsonEqual(history[index]["id"], targetRepr) || pyjsonEqual(history[index]["message_id"], targetRepr) {
					found, ordinal = true, int64(userOrdinal)
					break
				}
			}
			notFound = "target message_id %s not found in history for session %s"
		}
		if !found {
			logger().Warn(fmt.Sprintf("prompt.submit: "+notFound+"; refusing truncation without fallback", targetRepr, sid))
			return stale(nil)
		}
		if clientOrdinal != nil && *clientOrdinal != ordinal && !(prefixUserCount > 0 && *clientOrdinal == ordinal+int64(prefixUserCount)) {
			logger().Warn(fmt.Sprintf("prompt.submit: REFUSED truncation due to ordinal mismatch for session %s "+
				"(ordinal=%d, %s_ordinal=%d, %s=%s, prefix_user_count=%d). "+
				"Stale truncate_before_user_ordinal detected.",
				sid, *clientOrdinal, paramName, ordinal, paramName, targetRepr, prefixUserCount))
			return truncationCut{}, rpcErr(rid, 4030, fmt.Sprintf("truncate_before_user_ordinal (%d) does not match %s target turn (%d)",
				*clientOrdinal, paramName, ordinal), nil)
		}
	} else {
		ordinal = *clientOrdinal - int64(prefixUserCount)
		if ordinal < 0 || ordinal >= int64(len(userIndices)) {
			return stale(nil)
		}
		hasStampedUser := false
		for _, index := range userIndices {
			if _, ok := messageRowID(history[index]); ok {
				hasStampedUser = true
				break
			}
		}
		durable, durableOK := []map[string]any{}, true
		if !hasStampedUser {
			durable, durableOK = loadDurableTruncationHistory(ctx, session, sid, true)
		}
		if hasStampedUser || !durableOK || len(durable) > 0 {
			logger().Warn(fmt.Sprintf("prompt.submit: REFUSED ordinal-only truncation of durable "+
				"session %s (ordinal=%d); truncate_before_row_id required", sid, *clientOrdinal))
			return truncationCut{}, rpcErr(rid, 4004, "ordinal-only truncation is unsafe for durable session history; "+
				"include truncate_before_row_id", nil)
		}
	}
	if ordinal < 0 || ordinal >= int64(len(userIndices)) {
		return stale(&ordinal)
	}
	cut.ordinal, cut.cutIndex = int(ordinal), userIndices[ordinal]
	return cut, nil
}

// Durable row ids the messages carry
func rowIDsOf(messages []map[string]any) map[int64]bool {
	ids := map[int64]bool{}
	for _, message := range messages {
		if id, ok := messageRowID(message); ok {
			ids[id] = true
		}
	}
	return ids
}

// User turns a cut at the index drops, a repaired carrier counted from its physical rows and failing closed
func archivedUserTurns(ctx context.Context, session *tuigateway.Session, sid string, history []map[string]any, cutIndex int, survivorIDs map[int64]bool) int {
	var physical map[int64]map[string]any
	archived := 0
	for _, index := range historyUserIndices(history) {
		if index < cutIndex {
			continue
		}
		message := history[index]
		var absorbed []int64
		for _, raw := range historyList(message["_absorbed_row_ids"]) {
			if n, ok := pyint.Instance(raw); ok && n.IsInt64() {
				absorbed = append(absorbed, n.Int64())
			}
		}
		if len(absorbed) == 0 {
			archived++
			continue
		}
		if physical == nil {
			rows, _ := loadDurableTruncationHistory(ctx, session, sid, false)
			physical = map[int64]map[string]any{}
			for _, row := range rows {
				if id, ok := messageRowID(row); ok {
					physical[id] = row
				}
			}
		}
		ids := map[int64]bool{}
		own, ownOK := messageRowID(message)
		if !ownOK {
			archived++
		} else {
			ids[own] = true
		}
		for _, id := range absorbed {
			ids[id] = true
		}
		for id := range ids {
			if survivorIDs[id] {
				continue
			}
			row, found := physical[id]
			if !found || agent.UserOriginatedTurnView(row) != nil {
				archived++
			}
		}
	}
	return archived
}

// Rewind or regenerate cut under the history lock returning the client rowId rebind fields
func truncateHistoryForSubmit(ctx context.Context, rid any, sid string, session *tuigateway.Session, params map[string]any, requestedRebindIDs map[int64]bool) (map[string]any, map[string]any) {
	history := historyWithoutEphemeralScaffolding(historyList(session.Get("history")))
	cut, errResp := resolveTruncationOrdinal(ctx, rid, sid, session, params, history)
	if errResp != nil {
		return errResp, map[string]any{}
	}
	var truncated []map[string]any
	if cut.hasPrefix {
		truncated = make([]map[string]any, 0, len(cut.durablePrefix))
		for _, message := range cut.durablePrefix {
			truncated = append(truncated, copyDict(message))
		}
	} else {
		before, _, err := agent.HistoryBeforeUserOriginatedTurn(history, cut.cutIndex)
		panicIf(err)
		truncated = before
	}
	if len(truncated) == 0 && len(history) > 0 && !provider.IsTruthyValue(params["confirm_empty_truncate"]) {
		logger().Warn(fmt.Sprintf("prompt.submit: REFUSED empty truncation of session %s "+
			"(%d messages would be wiped; ordinal=%d).", sid, len(history), cut.ordinal))
		return rpcErr(rid, 4028, "truncation would erase the entire session transcript; "+
			"resubmit with confirm_empty_truncate=true if this is intended", nil), map[string]any{}
	}
	archivedTurns := archivedUserTurns(ctx, session, sid, history, cut.cutIndex, rowIDsOf(truncated))
	if archivedTurns > 1 && !provider.IsTruthyValue(params["confirm_deep_truncate"]) {
		archivedMessages := len(history) - len(truncated)
		if cut.hasPrefix {
			archivedMessages = len(history) - cut.cutIndex
		}
		verdict := "ALLOWED (warn-only release)"
		if deepTruncateEnforced {
			verdict = "REFUSED"
		}
		logger().Warn(fmt.Sprintf("prompt.submit: %s unconfirmed deep truncation of session %s (%d messages / %d user "+
			"turns; ordinal=%d).", verdict, sid, archivedMessages, archivedTurns, cut.ordinal))
		if deepTruncateEnforced {
			return rpcErr(rid, 4033, "truncation would archive later user turns; resubmit with "+
				"confirm_deep_truncate=true if this is intended",
				map[string]any{"archived_messages": archivedMessages, "archived_user_turns": archivedTurns}), map[string]any{}
		}
	}
	message := fmt.Sprintf("prompt.submit: truncating session %s history %d -> %d messages (ordinal=%d)", sid, len(history), len(truncated), cut.ordinal)
	if len(truncated) == 0 {
		logger().Warn(message)
	} else {
		logger().Info(message)
	}
	fields := map[string]any{}
	db, release := sessionDB(ctx, session)
	defer release()
	if db != nil {
		if failure := persistTruncation(ctx, db, sid, session, history, truncated, requestedRebindIDs, fields); failure != nil {
			logger().Error(fmt.Sprintf("prompt.submit: replace_messages failed for session %s (ordinal=%d); refusing "+
				"turn so memory and DB stay aligned: %s", sid, cut.ordinal, pyrepr.Str(failure)))
			return rpcErr(rid, 5008, "failed to persist history truncation: "+pyrepr.Str(failure), nil), map[string]any{}
		}
	}
	session.Set("history", messagesAsHistory(truncated))
	session.Set("history_version", intOr(session.Get("history_version"))+1)
	return nil, fields
}

// Writes the truncated transcript before memory changes and fills the survivor rebind fields, the failure when it raised
func persistTruncation(ctx context.Context, db *state.DB, sid string, session *tuigateway.Session, history, truncated []map[string]any, requestedRebindIDs map[int64]bool, fields map[string]any) any {
	var oldActiveRowIDs map[int64]bool
	var oldSurvivorRowIDs []*int64
	failure := catchPanic(func() {
		truncationKey := strOr(session.Get("session_key"))
		if truncationKey == "" {
			truncationKey = sid
		}
		oldActiveRowIDs = rowIDsOf(history)
		if requestedRebindIDs != nil {
			durable, ok := loadDurableTruncationHistory(ctx, session, truncationKey, false)
			if !ok {
				panic(errors.New("could not load durable row identities for truncation"))
			}
			for id := range rowIDsOf(durable) {
				oldActiveRowIDs[id] = true
			}
		}
		for _, message := range truncated {
			if id, ok := messageRowID(message); ok {
				oldSurvivorRowIDs = append(oldSurvivorRowIDs, &id)
			} else {
				oldSurvivorRowIDs = append(oldSurvivorRowIDs, nil)
			}
		}
		panicIf(db.ReplaceMessages(ctx, truncationKey, truncated, state.ReplaceOptions{ActiveOnly: true, ArchiveDropped: true, RejectActiveTurnLease: true}))
	})
	if failure != nil {
		return failure
	}
	if requestedRebindIDs == nil {
		survivors := []any{}
		for _, index := range historyUserIndices(truncated) {
			if id, ok := messageRowID(truncated[index]); ok {
				survivors = append(survivors, id)
			} else {
				survivors = append(survivors, nil)
			}
		}
		fields["survivor_user_row_ids"] = survivors
		return nil
	}
	rowIDMap := map[string]any{}
	for i, old := range oldSurvivorRowIDs {
		newID, ok := messageRowID(truncated[i])
		if old != nil && ok && requestedRebindIDs[*old] {
			rowIDMap[strconv.FormatInt(*old, 10)] = newID
		}
	}
	for dropped := range requestedRebindIDs {
		if oldActiveRowIDs[dropped] {
			if _, present := rowIDMap[strconv.FormatInt(dropped, 10)]; !present {
				rowIDMap[strconv.FormatInt(dropped, 10)] = nil
			}
		}
	}
	fields["survivor_row_id_map"] = rowIDMap
	return nil
}
