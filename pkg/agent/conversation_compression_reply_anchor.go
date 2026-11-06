package agent

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

type idSet map[string]bool

func (s idSet) equal(o idSet) bool {
	if len(s) != len(o) {
		return false
	}
	for k := range s {
		if !o[k] {
			return false
		}
	}
	return true
}

func (s idSet) intersects(o idSet) bool {
	for k := range s {
		if o[k] {
			return true
		}
	}
	return false
}

// Reinserts the newest visible assistant reply an engine folded away, returning the new list and the placed row
func ensureCompressedKeepsLastAssistantReply(ctx context.Context, original, compressed []map[string]any, sessionID string) ([]map[string]any, map[string]any) {
	var reply map[string]any
	replyPos, replyText := -1, ""
	for i := len(original) - 1; i >= 0; i-- {
		message := original[i]
		if message == nil || message["role"] != "assistant" {
			continue
		}
		if pyrepr.Truthy(message["tool_calls"]) || state.IsCompactionSummaryMessage(message) {
			continue
		}
		replyText = pystr.Strip(compressionMessageText(message))
		if replyText == "" {
			continue
		}
		reply, replyPos = message, i
		break
	}
	if reply == nil {
		return compressed, nil
	}
	isReplyTwin := func(message map[string]any) bool {
		if message == nil || message["role"] != "assistant" || pyrepr.Truthy(message["tool_calls"]) {
			return false
		}
		return sameVisibleContent(message, reply, replyText)
	}
	for _, message := range compressed {
		if isReplyTwin(message) {
			return compressed, nil
		}
	}
	anchor := freshCompactionMessageCopy(reply)
	index, ok := replyInsertionIndex(original[replyPos+1:], compressed, reusedToolCallIDs(original))
	session := sessionID
	if session == "" {
		session = "none"
	}
	if !ok {
		slog.Warn("Compression: engine folded away the just-delivered assistant reply and the surviving tool " +
			"rounds reuse tool-call ids, so its slot cannot be located; not reinserting it (session=" + session + ").")
		return compressed, nil
	}
	var neighbours []map[string]any
	for _, i := range []int{index - 1, index} {
		if i >= 0 && i < len(compressed) {
			neighbours = append(neighbours, compressed[i])
		}
	}
	for _, message := range neighbours {
		if isReplyTwin(message) {
			return compressed, nil
		}
	}
	for _, message := range neighbours {
		if message != nil && message["role"] == "assistant" {
			slog.Warn("Compression: engine folded away the just-delivered assistant reply and left a non-matching " +
				"assistant row at its slot; not reinserting it to keep strict role alternation (session=" + session + ").")
			return compressed, nil
		}
	}
	roughIn, roughOut := estimateRowsRough(ctx, original), estimateRowsRough(ctx, compressed)
	if roughOut < roughIn && roughIn < roughOut+estimateRowsRough(ctx, []map[string]any{anchor}) {
		slog.Info("Compression: not reinserting the just-delivered assistant reply; it would stop this " +
			"compaction from shrinking the transcript (session=" + session + ").")
		return compressed, nil
	}
	anchor[dbPersistedMarker] = true
	out := make([]map[string]any, 0, len(compressed)+1)
	out = append(out, compressed[:index]...)
	out = append(out, anchor)
	return append(out, compressed[index:]...), anchor
}

// Normalized text equality when either side has text, raw content equality otherwise
func sameVisibleContent(left, right map[string]any, rightText string) bool {
	leftText := pystr.Strip(compressionMessageText(left))
	if leftText != "" || rightText != "" {
		return leftText == rightText
	}
	return pyrepr.Equal(left["content"], right["content"])
}

// Rows of a tool_calls value whether held as a generic list or a dict list
func toolCallItems(v any) []any {
	switch calls := v.(type) {
	case []any:
		return calls
	case []map[string]any:
		out := make([]any, len(calls))
		for i, c := range calls {
			out[i] = c
		}
		return out
	}
	return nil
}

// Pairing ids of an assistant row's tool calls keyed call_id before id like the pre call sanitizer
func toolCallIDs(message map[string]any) idSet {
	out := idSet{}
	for _, tc := range toolCallItems(message["tool_calls"]) {
		if id := state.CoalesceToolCallID(tc); id != "" {
			out[id] = true
		}
	}
	return out
}

// Normalized tool_call_id of a tool row
func toolResultID(message map[string]any) string {
	return state.CoalesceToolCallID(map[string]any{"id": message["tool_call_id"]})
}

// Tool call ids more than one assistant row uses, which cannot locate a specific row
func reusedToolCallIDs(messages []map[string]any) idSet {
	seen := map[string]int{}
	for _, message := range messages {
		if message != nil && message["role"] == "assistant" {
			for id := range toolCallIDs(message) {
				seen[id]++
			}
		}
	}
	out := idSet{}
	for id, n := range seen {
		if n > 1 {
			out[id] = true
		}
	}
	return out
}

// Chronological slot for the dropped reply before its last surviving follower, false when reused ids hide it
func replyInsertionIndex(followers, compressed []map[string]any, reusedIDs idSet) (int, bool) {
	rowIDs := func(message map[string]any) idSet {
		ids := toolCallIDs(message)
		if id := toolResultID(message); id != "" {
			ids[id] = true
		}
		return ids
	}
	var kept []map[string]any
	for _, f := range followers {
		if f != nil {
			kept = append(kept, f)
		}
	}
	ambiguous := false
	for _, follower := range kept {
		followerRole, followerText := follower["role"], pystr.Strip(compressionMessageText(follower))
		followerCallIDs := toolCallIDs(follower)
		if rowIDs(follower).intersects(reusedIDs) {
			if !ambiguous {
				for _, m := range compressed {
					if m != nil && rowIDs(m).intersects(reusedIDs) {
						ambiguous = true
						break
					}
				}
			}
			continue
		}
		for pos := len(compressed) - 1; pos >= 0; pos-- {
			message := compressed[pos]
			if message == nil || !pyrepr.Equal(message["role"], followerRole) {
				continue
			}
			messageCallIDs := toolCallIDs(message)
			if len(followerCallIDs) > 0 || len(messageCallIDs) > 0 {
				if messageCallIDs.equal(followerCallIDs) {
					return pos, true
				}
				continue
			}
			if followerRole == "tool" && (toolResultID(follower) != "" || toolResultID(message) != "") {
				if toolResultID(message) == toolResultID(follower) {
					return pos, true
				}
				continue
			}
			if sameVisibleContent(message, follower, followerText) {
				return pos, true
			}
		}
	}
	if ambiguous {
		return 0, false
	}
	if len(kept) > 0 && len(compressed) > 0 && isRealUserMessage(compressed[len(compressed)-1]) {
		for _, f := range kept {
			if isRealUserMessage(f) {
				return len(compressed) - 1, true
			}
		}
	}
	return len(compressed), true
}
