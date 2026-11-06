package state

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	CompressedSummaryMetadataKey    = "_compressed_summary"
	CompressedSummaryHasUserTurnKey = "_compressed_summary_has_user_turn"
	MicroCompactMarkerKey           = "_micro_compact_marker"
	SteerDisplayKind                = "steer"

	CompressionContinuationUserContent       = "Continue from the compressed conversation context above. This marker exists because no human user turn was available."
	legacyCompressionContinuationUserContent = "Continue from the compressed conversation context above. This marker exists because the compacted transcript contained no preserved user turn."
	MaxIterationsSummaryRequest              = "You've reached the maximum number of tool-calling iterations allowed. Please provide a final response summarizing what you've found and accomplished so far, without calling any more tools."
	BackgroundProcessNotificationPrefix      = "[IMPORTANT: Background process "
	TodoInjectionHeader                      = "[Your active task list was preserved across context compression]"

	LengthContinuationNetworkStub        = "[System: The previous response was cut off by a network error mid-stream — a transport interruption, NOT a change in your capabilities. Your tools are still fully available; call them as normal and ignore any earlier claim that you lack tool access. Continue the task from where you left off. Do not restart or repeat prior text.]"
	LengthContinuationOutputLimit        = "[System: Your previous response was truncated by the output length limit. Continue exactly where you left off. Do not restart or repeat prior text. Finish the answer directly.]"
	LegacyLengthContinuationNetworkStub  = "[System: The previous response was cut off by a network error mid-stream. Continue exactly where you left off. Do not restart or repeat prior text. Finish the answer directly.]"
	LengthContinuationDroppedToolsPrefix = "[System: Your previous tool call "
	CodexIncompleteNudge                 = "[System: Your previous response contained only internal reasoning and never produced a visible answer or tool call. Do not keep thinking. Produce your final answer as plain text now (or make the tool call you were planning).]"
	CodexAckContinuationNudge            = "[System: Continue now. Execute the required tool calls and only send your final answer after completing the task.]"
	DegenerateFinalNudge                 = "[System: Your previous message ended the turn with a fragment that is not a usable answer. If the task is unfinished, continue it and then give the complete answer. If that fragment WAS your complete answer, send it again exactly as before.]"
	DroppedToolcallNudgeContent          = "Your previous turn indicated a tool call but none was included. Do not narrate a plan or restate intent — issue the actual tool call now to continue the task."
	EmptyToolResponseNudge               = "You just executed tool calls but returned an empty response. Please process the tool results above and continue with the task."
)

// Display metadata keys a summary carrier keeps on its live projection
var SummaryCarrierDurableDisplayMetadataKeys = []string{"reactions"}

var syntheticUserTurnTexts = setOf(CompressionContinuationUserContent, legacyCompressionContinuationUserContent,
	MaxIterationsSummaryRequest, CodexIncompleteNudge, CodexAckContinuationNudge, DegenerateFinalNudge,
	DroppedToolcallNudgeContent, EmptyToolResponseNudge, LengthContinuationNetworkStub,
	LegacyLengthContinuationNetworkStub, LengthContinuationOutputLimit)

var syntheticUserTurnPrefixes = []string{BackgroundProcessNotificationPrefix, TodoInjectionHeader + "\n", LengthContinuationDroppedToolsPrefix}

// Copy of a content part carrying text, string parts becoming the text itself
func withPartText(item any, text string) any {
	switch m := item.(type) {
	case map[string]any:
		out := make(map[string]any, len(m)+1)
		for k, v := range m {
			out[k] = v
		}
		out["text"] = text
		return out
	case pyObject:
		out := append(pyObject{}, m...)
		for i := range out {
			if out[i].Key == "text" {
				out[i].Value = text
				return out
			}
		}
		return append(out, pyKeyValue{"text", text})
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range m.Keys {
			out.Set(k, m.Vals[k])
		}
		out.Set("text", text)
		return out
	}
	return text
}

// Shallow copy of a dict content part, other parts unchanged
func copyPart(item any) any {
	switch m := item.(type) {
	case map[string]any:
		return copyMessage(m)
	case pyObject:
		return append(pyObject{}, m...)
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range m.Keys {
			out.Set(k, m.Vals[k])
		}
		return out
	}
	return item
}

// Shallow copy of a message dict
func copyMessage(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Deep copy of a JSON shaped value
func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopyValue(val)
		}
		return out
	case pyObject:
		out := make(pyObject, len(x))
		for i, kv := range x {
			out[i] = pyKeyValue{kv.Key, deepCopyValue(kv.Value)}
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys {
			out.Set(k, deepCopyValue(x.Vals[k]))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = deepCopyValue(val)
		}
		return out
	}
	return v
}

// Whether the message carries the in process compressed summary flag
func hasCompressedSummaryMetadata(message map[string]any) bool {
	return message != nil && pyTruthy(message[CompressedSummaryMetadataKey])
}

// Summary handoff message by metadata or content
func isContextSummaryMessage(message map[string]any) bool {
	if message == nil {
		return false
	}
	return hasCompressedSummaryMetadata(message) || IsContextSummaryContent(message["content"])
}

// Context compaction handoff summary by metadata key or content heuristics
func IsCompactionSummaryMessage(message any) bool {
	if m, ok := message.(map[string]any); ok {
		return isContextSummaryMessage(m)
	}
	return IsContextSummaryContent(message)
}

// Empty non summary user role echo
func isBlankUserTurn(message map[string]any) bool {
	if message == nil || message["role"] != "user" || isContextSummaryMessage(message) {
		return false
	}
	content := message["content"]
	if content == nil {
		return true
	}
	if s, ok := content.(string); ok {
		return pyStrip(s) == ""
	}
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		switch p := part.(type) {
		case string:
			if pyStrip(p) != "" {
				return false
			}
		default:
			if !isMapping(p) {
				return false
			}
			kind, _ := mappingGet(p, "type")
			if kind != "text" && kind != "input_text" {
				return false
			}
			text, _ := mappingGet(p, "text")
			s, isStr := text.(string)
			if !isStr || pyStrip(s) != "" {
				return false
			}
		}
	}
	return true
}

// User turn carrying input worth anchoring, a steer row counting as human input
func isActionableUserTurn(message map[string]any) bool {
	if message == nil || message["role"] != "user" {
		return false
	}
	kind := message["display_kind"]
	if (pyTruthy(kind) && kind != SteerDisplayKind) || isContextSummaryMessage(message) {
		return false
	}
	return !isBlankUserTurn(message)
}

// Internal user role scaffolding recognised by summary metadata or a recovery nudge marker
func isSyntheticCompressionUserTurn(message map[string]any) bool {
	if message == nil || message["role"] != "user" {
		return false
	}
	if isContextSummaryMessage(message) {
		return true
	}
	text := pyStrip(ContentTextForContains(message["content"]))
	if syntheticUserTurnTexts[text] {
		return true
	}
	for _, prefix := range syntheticUserTurnPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// Actionable user turn that is not synthetic compression scaffolding
func IsRealUserTurn(message map[string]any) bool {
	return isActionableUserTurn(message) && !isSyntheticCompressionUserTurn(message)
}

// Copy without stale handoff data keeping merged prior tail content, nil for a standalone handoff
func StripContextSummaryHandoffMessage(message map[string]any) map[string]any {
	if message == nil {
		return nil
	}
	if !isContextSummaryMessage(message) {
		return copyMessage(message)
	}
	unwrapped := func(content any) map[string]any {
		out := copyMessage(message)
		out["content"] = content
		delete(out, CompressedSummaryMetadataKey)
		return out
	}
	switch content := message["content"].(type) {
	case string:
		prior := ""
		if i := strings.Index(content, MergedSummaryDelimiter); i >= 0 {
			prior = pyStrip(content[:i])
			if strings.HasPrefix(prior, MergedPriorContextHeader) {
				prior = pyLstrip(prior[len(MergedPriorContextHeader):])
			}
		} else if i := strings.Index(content, SummaryEndMarker); i >= 0 {
			prior = pyLstrip(content[i+len(SummaryEndMarker):])
		}
		if prior == "" {
			return nil
		}
		return unwrapped(prior)
	case []any:
		var priorBlocks []any
		foundDelimiter := false
		for _, item := range content {
			text, isStr := partText(item).(string)
			if isStr && strings.Contains(text, MergedSummaryDelimiter) {
				before := text[:strings.Index(text, MergedSummaryDelimiter)]
				if pyStrip(before) != "" {
					priorBlocks = append(priorBlocks, withPartText(item, before))
				}
				foundDelimiter = true
				break
			}
			priorBlocks = append(priorBlocks, copyPart(item))
		}
		if !foundDelimiter {
			for index, item := range content {
				text, isStr := partText(item).(string)
				if isStr && strings.Contains(text, SummaryEndMarker) {
					remainder := pyLstrip(text[strings.Index(text, SummaryEndMarker)+len(SummaryEndMarker):])
					var legacy []any
					if remainder != "" {
						legacy = append(legacy, withPartText(item, remainder))
					}
					for _, later := range content[index+1:] {
						legacy = append(legacy, copyPart(later))
					}
					if len(legacy) == 0 {
						return nil
					}
					return unwrapped(legacy)
				}
			}
			return nil
		}
		for index, item := range priorBlocks {
			text, isStr := partText(item).(string)
			if isStr && strings.HasPrefix(pyLstrip(text), MergedPriorContextHeader) {
				leading := pyLstrip(pyLstrip(text)[len(MergedPriorContextHeader):])
				if leading != "" {
					priorBlocks[index] = withPartText(item, leading)
				} else {
					priorBlocks = append(priorBlocks[:index], priorBlocks[index+1:]...)
				}
				break
			}
		}
		if len(priorBlocks) == 0 {
			return nil
		}
		return unwrapped(priorBlocks)
	}
	return nil
}

// Summary bearing content projected to the synthetic handoff alone, never keeping live media
func handoffOnlyContent(content any) any {
	throughEndMarker := func(text string) string {
		if i := strings.Index(text, SummaryEndMarker); i >= 0 {
			return text[:i+len(SummaryEndMarker)]
		}
		return text
	}
	switch c := content.(type) {
	case string:
		if i := strings.Index(c, MergedSummaryDelimiter); i >= 0 {
			c = pyLstrip(c[i+len(MergedSummaryDelimiter):])
		}
		return throughEndMarker(c)
	case []any:
		for _, item := range c {
			text, isStr := partText(item).(string)
			if !isStr || !strings.Contains(text, MergedSummaryDelimiter) {
				continue
			}
			suffix := throughEndMarker(pyLstrip(text[strings.Index(text, MergedSummaryDelimiter)+len(MergedSummaryDelimiter):]))
			if suffix == "" {
				return []any{}
			}
			return []any{withPartText(item, suffix)}
		}
		projected := []any{}
		for _, item := range c {
			text, isStr := partText(item).(string)
			if !isStr {
				continue
			}
			if i := strings.Index(text, SummaryEndMarker); i >= 0 {
				return append(projected, withPartText(item, text[:i]+SummaryEndMarker))
			}
			projected = append(projected, copyPart(item))
		}
		return projected
	}
	return content
}

// Drops the api_content sidecar of a message whose content was rewritten
func DropStaleAPIContent(msg map[string]any) { delete(msg, "api_content") }

// User row split into its handoff only part and its live human view, either nil, both fresh dicts
func SplitUserOriginatedTurn(message map[string]any) (map[string]any, map[string]any) {
	if message == nil || message["role"] != "user" {
		return nil, nil
	}
	isSummary := IsCompactionSummaryMessage(message)
	var handoff, candidate map[string]any
	if isSummary {
		handoff = map[string]any{"role": "user", "content": handoffOnlyContent(message["content"]),
			CompressedSummaryMetadataKey: true, "display_kind": "hidden"}
		if v, ok := message[CompressedSummaryHasUserTurnKey]; ok {
			handoff[CompressedSummaryHasUserTurnKey] = pyTruthy(v)
		}
		if pyTruthy(message[MicroCompactMarkerKey]) {
			handoff[MicroCompactMarkerKey] = true
		}
		if ts := message["timestamp"]; ts != nil {
			handoff["timestamp"] = ts
		}
		DropStaleAPIContent(handoff)
		kind := message["display_kind"]
		if !pyTruthy(kind) || kind == "hidden" {
			candidate = StripContextSummaryHandoffMessage(message)
		}
		if candidate == nil {
			return handoff, nil
		}
	} else if kind := message["display_kind"]; pyTruthy(kind) && kind != SteerDisplayKind {
		return nil, nil
	} else {
		candidate = copyMessage(message)
	}
	drop := []string{CompressedSummaryMetadataKey, CompressedSummaryHasUserTurnKey, MicroCompactMarkerKey, DBPersistedMarker}
	if isSummary {
		drop = append(drop, "_row_id")
	}
	for _, key := range append(drop, "display_kind", "display_metadata") {
		delete(candidate, key)
	}
	if carrier := message["display_metadata"]; isMapping(carrier) {
		durable := map[string]any{}
		for _, key := range SummaryCarrierDurableDisplayMetadataKeys {
			if v, ok := mappingGet(carrier, key); ok {
				durable[key] = deepCopyValue(v)
			}
		}
		if len(durable) > 0 {
			candidate["display_metadata"] = durable
		}
	}
	DropStaleAPIContent(candidate)
	if !IsRealUserTurn(candidate) {
		return handoff, nil
	}
	return handoff, candidate
}
