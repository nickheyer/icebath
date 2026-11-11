package tuiserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const staleTargetMsg = "target user message is no longer in session history"

const groupProbeFailedMsg = "Could not verify this group. Try again after the gateway recovers."

// Indices of canonical live user turns, composite carriers included
func historyUserIndices(history []map[string]any) []int {
	var out []int
	for i, m := range history {
		if agent.UserOriginatedTurnView(m) != nil {
			out = append(out, i)
		}
	}
	return out
}

// Durable row id of a history entry from _row_id else row_id
func messageRowID(msg map[string]any) (int64, bool) {
	raw, present := msg["_row_id"]
	if !present || raw == nil {
		raw = msg["row_id"]
	}
	if raw == nil {
		return 0, false
	}
	n, err := pyint.Int64(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Whether a live memory entry plausibly corresponds to a durable row by role, marker status and user text
func memDBPairAgrees(mem, dbMsg map[string]any) bool {
	if mem == nil || dbMsg == nil {
		return false
	}
	if !pyjson.Equal(mem["role"], dbMsg["role"]) {
		return false
	}
	if mem["role"] == "user" {
		memView, dbView := agent.UserOriginatedTurnView(mem), agent.UserOriginatedTurnView(dbMsg)
		if (memView == nil) != (dbView == nil) {
			return false
		}
		if memView == nil {
			return truthy(mem["display_kind"]) == truthy(dbMsg["display_kind"])
		}
		memContent, memText := memView["content"].(string)
		dbContent, dbText := dbView["content"].(string)
		if memText && dbText {
			return pystr.Strip(memory.SanitizeContext(memContent)) == pystr.Strip(memory.SanitizeContext(dbContent))
		}
		return true
	}
	return truthy(mem["display_kind"]) == truthy(dbMsg["display_kind"])
}

// User ordinal and history index of the exact row id match, ok false when none
func findUserTurnByRowID(history []map[string]any, targetRowID int64) (int, int, bool) {
	for ord, idx := range historyUserIndices(history) {
		if rid, has := messageRowID(history[idx]); has && rid == targetRowID {
			return ord, idx, true
		}
	}
	return 0, 0, false
}

// Durable live replay transcript of the session, ok false when it cannot be proven safe
func loadDurableTruncationHistory(ctx context.Context, session *tuigateway.Session, fallbackSID string, repairAlternation bool) ([]map[string]any, bool) {
	sessionKey := submitRowTargetKey(session)
	if sessionKey == "" {
		sessionKey = fallbackSID
	}
	if sessionKey == "" {
		return []map[string]any{}, true
	}
	var history []map[string]any
	failure := catchPanic(func() {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			panic(fmt.Errorf("no session store"))
		}
		rows, err := db.GetMessagesAsConversation(ctx, sessionKey, state.ConversationOptions{RepairAlternation: repairAlternation, IncludeRowIDs: true})
		if err != nil {
			panic(err)
		}
		history = rows
	})
	if failure != nil {
		logger().Debug("prompt.submit: failed loading durable history for session "+sessionKey, "error", pyrepr.Str(failure))
		return nil, false
	}
	if history == nil {
		history = []map[string]any{}
	}
	return history, true
}

// Truncate row id resolved against the live list, the durable prefix set when a carrier absorbed the target
type truncateTarget struct {
	ordinal, index int
	durablePrefix  []map[string]any
	hasPrefix      bool
}

// Resolves truncate_before_row_id against the live list, healing missing stamps from an aligned durable copy
func resolveTruncateRowID(ctx context.Context, session *tuigateway.Session, history []map[string]any, targetRowID int64) (truncateTarget, bool) {
	if ord, idx, ok := findUserTurnByRowID(history, targetRowID); ok {
		return truncateTarget{ordinal: ord, index: idx}, true
	}
	dbHistory, ok := loadDurableTruncationHistory(ctx, session, "", false)
	if !ok {
		return truncateTarget{}, false
	}
	if len(dbHistory) == len(history) {
		agree := true
		for i := range history {
			if !memDBPairAgrees(history[i], dbHistory[i]) {
				agree = false
				break
			}
		}
		if agree {
			for i := range history {
				if dbRID, has := messageRowID(dbHistory[i]); has {
					if _, memHas := messageRowID(history[i]); !memHas {
						history[i]["_row_id"] = dbRID
					}
				}
			}
			if ord, idx, found := findUserTurnByRowID(history, targetRowID); found {
				return truncateTarget{ordinal: ord, index: idx}, true
			}
		}
	}
	dbOrd, dbIdx, found := findUserTurnByRowID(dbHistory, targetRowID)
	if !found {
		return truncateTarget{}, false
	}
	memUserIndices := historyUserIndices(history)
	if dbOrd < len(memUserIndices) && memDBPairAgrees(history[memUserIndices[dbOrd]], dbHistory[dbIdx]) {
		return truncateTarget{ordinal: dbOrd, index: memUserIndices[dbOrd]}, true
	}
	for ord, idx := range memUserIndices {
		carrier := history[idx]
		absorbed := false
		if ids, isList := carrier["_absorbed_row_ids"].([]any); isList {
			for _, rid := range ids {
				if n, isInt := pyrepr.Int(rid); isInt && n.IsInt64() && n.Int64() == targetRowID {
					if _, isBool := rid.(bool); !isBool {
						absorbed = true
					}
				}
			}
		}
		carrierRID, carrierHas := messageRowID(carrier)
		if !(carrierHas && carrierRID == targetRowID) && !absorbed {
			continue
		}
		if !mergeCarrierHoldsTargetText(carrier, dbHistory[dbIdx]) {
			continue
		}
		return truncateTarget{ordinal: ord, index: idx, durablePrefix: dbHistory[:dbIdx], hasPrefix: true}, true
	}
	return truncateTarget{}, false
}

// Whether a live merge carrier holds the durable target row's text whole or as a blank line delimited part
func mergeCarrierHoldsTargetText(carrier, dbMsg map[string]any) bool {
	memView, dbView := agent.UserOriginatedTurnView(carrier), agent.UserOriginatedTurnView(dbMsg)
	if memView == nil || dbView == nil {
		return false
	}
	memContent, memText := memView["content"].(string)
	dbContent, dbText := dbView["content"].(string)
	if !memText || !dbText {
		return false
	}
	memStripped := pystr.Strip(memory.SanitizeContext(memContent))
	dbStripped := pystr.Strip(memory.SanitizeContext(dbContent))
	return dbStripped == memStripped || strings.Contains(memStripped, "\n\n"+dbStripped)
}

// Integer client param refusing bools and non integers with a 4004 error response
func coerceTruncateInt(rid, value any, paramName string) (int64, map[string]any) {
	if _, isBool := value.(bool); !isBool {
		if n, err := pyint.Int64(value); err == nil {
			return n, nil
		}
	}
	return 0, rpcErr(rid, 4004, paramName+" must be an integer", nil)
}

// Note block of the reactions since the last turn, empty unless display.message_reactions is on
func pendingReactionNotes(ctx context.Context, session *tuigateway.Session) string {
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return ""
	}
	display, ok := loadCfg(ctx)["display"].(map[string]any)
	if !ok || !truthy(display["message_reactions"]) {
		return ""
	}
	var pending []map[string]any
	if failure := catchPanic(func() {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return
		}
		rows, err := db.TakeUnseenReactions(ctx, sessionKey, "user")
		if err != nil {
			panic(err)
		}
		pending = rows
	}); failure != nil {
		logger().Debug("Failed to read pending reactions", "error", pyrepr.Str(failure))
		return ""
	}
	var notes []string
	for _, entry := range pending {
		snippet := strings.ReplaceAll(pystr.Strip(pyrepr.Str(orEmpty(entry["text"]))), "\n", " ")
		if pystr.Len(snippet) > 120 {
			snippet = pystr.Slice(snippet, 0, 120) + "…"
		}
		emoji := pyrepr.Str(orEmpty(entry["emoji"]))
		whose := "your"
		if entry["role"] == "user" {
			whose = "their own"
		}
		if snippet != "" {
			notes = append(notes, "[The user reacted "+emoji+" to "+whose+" message: \""+snippet+"\"]")
		} else {
			notes = append(notes, "[The user reacted "+emoji+" to "+whose+" earlier message]")
		}
	}
	return strings.Join(notes, "\n")
}

// Live session row an off turn submit write targets, the rotated agent session id else the session key
func submitRowTargetKey(session *tuigateway.Session) string {
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		return a.SessionID()
	}
	return strOr(session.Get("session_key"))
}
