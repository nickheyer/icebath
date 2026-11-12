package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func agentRuntimeHelpersLog() *slog.Logger {
	return slog.Default().With("logger", "agent.agent_runtime_helpers")
}

const (
	emptyHealEscalateAfter = 3
	emptyHealWindowS       = 600.0
	emptyHealMaxSessions   = 1024
)

var validAPIRoles = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true, "function": true, "developer": true}

type emptyHealState struct {
	count, totalEvents, totalHealed int
	windowStart                     float64
	escalated                       bool
}

var (
	emptyHealLogLock       sync.Mutex
	emptyHealLogState      = map[string]*emptyHealState{}
	emptyHealLogOrder      []string
	emptyHealUserNotified  = map[string]bool{}
	emptyHealPendingNotice = map[string]string{}
	inflightTurnsLock      sync.Mutex
	inflightTurnsBySession = map[string]inflightTurn{}
	monotonicStart         = time.Now()
)

type inflightTurn struct {
	turnID  string
	started float64
}

// Length of the pointer identical prefix the previous scan already validated
func cursorSkipPrefix(messages []*state.Message, cursor map[string]any) int {
	prev, _ := cursor["prefix"].([]*state.Message)
	start := 0
	for start < min(len(prev), len(messages)) && messages[start] == prev[start] {
		start++
	}
	return start
}

// Tool result answering the call within the tool run that starts at start, nil when none
func findToolResult(messages []*state.Message, start int, tc state.ToolCall) *state.Message {
	for _, candidate := range messages[start:] {
		if candidate == nil || candidate.Role != "tool" {
			return nil
		}
		if idSetsIntersect(state.ToolResultIDVariants(candidate.ToolCallID), state.ToolCallIDVariants(tc)) {
			return candidate
		}
	}
	return nil
}

// Puts the corruption marker ahead of an existing tool result once and clears its persisted mark
func prependCorruptionMarker(toolMsg *state.Message, marker string) {
	var existing string
	switch c := toolMsg.Content.(type) {
	case nil:
	case string:
		if strings.HasPrefix(c, marker) {
			return
		}
		existing = c
	default:
		if s, err := pyjson.Dumps(c); err == nil {
			existing = s
		} else {
			existing = pyrepr.Str(c)
		}
	}
	if existing != "" {
		toolMsg.Content = marker + "\n" + existing
	} else {
		toolMsg.Content = marker
	}
	toolMsg.Persisted = false
}

// Repairs corrupted assistant tool call argument JSON in place, skipping the prefix scanned last call
func sanitizeToolCallArguments(messages *[]*state.Message, logger *slog.Logger, sessionID string, cursor map[string]any) int {
	log := logger
	if log == nil {
		log = agentRuntimeHelpersLog()
	}
	repaired := 0
	msgs := *messages
	for idx := cursorSkipPrefix(msgs, cursor); idx < len(msgs); idx++ {
		msg := msgs[idx]
		if msg == nil || msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		insertAt := idx + 1
		for i := range msg.ToolCalls {
			tc := &msg.ToolCalls[i]
			arguments := tc.Function.Arguments
			if pystr.Strip(arguments) == "" {
				tc.Function.Arguments = "{}"
				msg.Persisted = false
				continue
			}
			if _, err := pyjson.Loads(arguments); err == nil {
				continue
			}
			toolCallID := getToolCallID(*tc)
			functionName := tc.Function.Name
			if functionName == "" {
				functionName = "?"
			}
			log.Warn(fmt.Sprintf("Corrupted tool_call arguments repaired before request (session=%s, message_index=%d, tool_call_id=%s, function=%s, original_arguments=%s)",
				orDash(sessionID), idx, orDash(toolCallID), functionName, pyrepr.Repr(truncateRunes(arguments, fullArgsLogBound))))
			tc.Function.Arguments = "{}"
			msg.Persisted = false
			if existing := findToolResult(msgs, idx+1, *tc); existing == nil {
				name := functionName
				if name == "?" {
					name = ""
				}
				msgs = slices.Insert(msgs, insertAt, tools.MakeToolResultMessage(name, toolCallArgumentsCorruptionMarker, toolCallID, ""))
				insertAt++
			} else {
				prependCorruptionMarker(existing, toolCallArgumentsCorruptionMarker)
			}
			repaired++
		}
	}
	*messages = msgs
	if cursor != nil {
		cursor["prefix"] = slices.Clone(msgs)
	}
	return repaired
}

const fullArgsLogBound = 100000

// First n code points of s, the Python slice of a str
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Warns when a turn starts while an earlier turn of the agent or session has not persisted, returns that turn
func noteTurnStart(a *AIAgent, turnID string) string {
	prev, prevStarted := a.inflightTurnID, a.inflightTurnStarted
	a.inflightTurnID = turnID
	a.inflightTurnStarted = nowSeconds()
	overlap := ""
	if prev != "" && prev != turnID {
		ago := -1.0
		if prevStarted != 0 {
			ago = nowSeconds() - prevStarted
		}
		agentRuntimeHelpersLog().Warn(fmt.Sprintf("turn %s starting while turn %s (started %.0fs ago) has not completed its turn-end persist (session=%s) — concurrent turns on one session; transcript writes may interleave",
			turnID, prev, ago, orDash(a.sessionID)))
		overlap = prev
	}
	if a.sessionID != "" && !a.persistDisabled {
		now := nowSeconds()
		inflightTurnsLock.Lock()
		entry, had := inflightTurnsBySession[a.sessionID]
		inflightTurnsBySession[a.sessionID] = inflightTurn{turnID, now}
		inflightTurnsLock.Unlock()
		a.inflightTurnSessionID = a.sessionID
		if had && entry.turnID != turnID && entry.turnID != prev {
			ago := -1.0
			if entry.started != 0 {
				ago = now - entry.started
			}
			agentRuntimeHelpersLog().Warn(fmt.Sprintf("turn %s starting while turn %s (started %.0fs ago) is still in flight on session %s under a different agent object — two routing keys are mapped to one session_id; concurrent turns on one session; transcript writes may interleave",
				turnID, entry.turnID, ago, a.sessionID))
			if overlap == "" {
				overlap = entry.turnID
			}
		}
	}
	return overlap
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Merged content of two adjacent user messages, false for shapes that cannot merge
func mergeUserContent(prev, cur any) (any, bool) {
	ps, pIsStr := prev.(string)
	cs, cIsStr := cur.(string)
	pl, pIsList := prev.([]any)
	cl, cIsList := cur.([]any)
	switch {
	case pIsStr && cIsStr:
		sep := ""
		if ps != "" && cs != "" {
			sep = "\n\n"
		}
		return ps + sep + cs, true
	case pIsList && cIsList:
		return append(slices.Clone(pl), cl...), true
	case pIsList && cIsStr:
		out := slices.Clone(pl)
		if cs != "" {
			out = append(out, map[string]any{"type": "text", "text": cs})
		}
		return out, true
	case pIsStr && cIsList:
		var out []any
		if ps != "" {
			out = append(out, map[string]any{"type": "text", "text": ps})
		}
		return append(out, cl...), true
	}
	return nil, false
}

// Drops thinking only assistant turns and nudge rows from the wire copy then merges adjacent user rows
func dropThinkingOnlyAndMergeUsers(messages []map[string]any, dropCodexReasoningItems bool, dropNudgeMarker *string) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	var kept []map[string]any
	for _, m := range messages {
		if dropNudgeMarker != nil && m["role"] == "user" && m["content"] == any(*dropNudgeMarker) {
			continue
		}
		if isThinkingOnlyAssistant(m, dropCodexReasoningItems) {
			continue
		}
		kept = append(kept, m)
	}
	dropped := len(messages) - len(kept)
	var merged []map[string]any
	merges := 0
	for _, m := range kept {
		if n := len(merged); n > 0 && merged[n-1]["role"] == "user" && m["role"] == "user" {
			prev := merged[n-1]
			if content, ok := mergeUserContent(dictGetOr(prev, "content", ""), dictGetOr(m, "content", "")); ok {
				cp := make(map[string]any, len(prev))
				for k, v := range prev {
					cp[k] = v
				}
				cp["content"] = content
				merged[n-1] = cp
				merges++
				continue
			}
		}
		merged = append(merged, m)
	}
	if dropped == 0 && merges == 0 {
		return messages
	}
	runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: dropped %d thinking-only assistant turn(s), merged %d adjacent user message(s)", dropped, merges))
	return merged
}

func dictGetOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Writes the interrupted placeholder onto an empty non final wire copy, true when filled
func fillEmptyNonFinalWirePayload(msg map[string]any, isFinal bool) bool {
	if isFinal || msg == nil || (msg["role"] != "user" && msg["role"] != "assistant") {
		return false
	}
	if state.MsgHasPayload(msg) {
		return false
	}
	msg["content"] = InterruptedPlaceholder
	return true
}

// Escalation threshold from agent.sanitizer_heal_escalation_threshold, the module default on any read error
func healEscalationThreshold(ctx context.Context) int {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return emptyHealEscalateAfter
	}
	agentCfg, _ := cfg.Get("agent").(map[string]any)
	raw, ok := agentCfg["sanitizer_heal_escalation_threshold"]
	if !ok || raw == nil {
		return emptyHealEscalateAfter
	}
	if n, ok := pythonInt(raw); ok {
		return n
	}
	return emptyHealEscalateAfter
}

var pyIntLiteral = regexp.MustCompile(`^[+-]?[0-9](_?[0-9])*$`)

// Python int() of a config scalar, false where int() raises
func pythonInt(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(math.Trunc(x)), true
	case float32:
		return pythonInt(float64(x))
	case string:
		s := pystr.Strip(x)
		if !pyIntLiteral.MatchString(s) {
			return 0, false
		}
		n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
		return n, err == nil
	}
	if b, ok := pyrepr.Int(v); ok && b.IsInt64() {
		return int(b.Int64()), true
	}
	return 0, false
}

// Drains the one time repeated heal notice for the session at most once per session lifetime
func consumePendingSanitizerHealNotice(logSessionID string) string {
	key := orDash(logSessionID)
	emptyHealLogLock.Lock()
	defer emptyHealLogLock.Unlock()
	notice := emptyHealPendingNotice[key]
	delete(emptyHealPendingNotice, key)
	return notice
}

// Per session heal counters heal_events, messages_healed and escalated for diagnostics
func getSanitizerHealStats() map[string]map[string]any {
	emptyHealLogLock.Lock()
	defer emptyHealLogLock.Unlock()
	out := make(map[string]map[string]any, len(emptyHealLogState))
	for k, v := range emptyHealLogState {
		out[k] = map[string]any{"heal_events": v.totalEvents, "messages_healed": v.totalHealed, "escalated": emptyHealUserNotified[k]}
	}
	return out
}

// Warns on the first heals in a window, errors once at the threshold and queues the one time notice
func logEmptyNonFinalHeal(ctx context.Context, logSessionID string, healed int) {
	key := orDash(logSessionID)
	threshold := healEscalationThreshold(ctx)
	now := time.Since(monotonicStart).Seconds()
	emptyHealLogLock.Lock()
	st := emptyHealLogState[key]
	delete(emptyHealLogState, key)
	if st == nil || now-st.windowStart > emptyHealWindowS {
		prior := st
		st = &emptyHealState{windowStart: now}
		if prior != nil {
			st.totalEvents, st.totalHealed = prior.totalEvents, prior.totalHealed
		}
	}
	provider.PublishLRU(emptyHealLogState, &emptyHealLogOrder, key, st, emptyHealMaxSessions)
	st.count++
	st.totalEvents++
	st.totalHealed += healed
	count, totalEvents, totalHealed := st.count, st.totalEvents, st.totalHealed
	if st.escalated {
		emptyHealLogLock.Unlock()
		return
	}
	escalate := threshold > 0 && count >= threshold
	if escalate {
		st.escalated = true
		if !emptyHealUserNotified[key] {
			emptyHealUserNotified[key] = true
			emptyHealPendingNotice[key] = fmt.Sprintf("⚠️ Your session transcript required repeated repair (%d heal passes so far). Replies keep working, but a corrupted turn is stuck in this session's history — run /debug share or `icebath doctor` to capture diagnostics, or /new to start a clean session.", totalEvents)
		}
	}
	emptyHealLogLock.Unlock()
	if escalate {
		runAgentLog().Error(fmt.Sprintf("Pre-call sanitizer: repeated-heal escalation for session %s — healed %d empty non-final message(s) this send; heal pattern: %d heal events / %d messages healed this session (%d in the current session window, threshold %d). The transcript is being repaired on every send; /new drops the poisoned turns.",
			key, healed, totalEvents, totalHealed, count, threshold))
		return
	}
	runAgentLog().Warn(fmt.Sprintf("Pre-call sanitizer: healed %d empty non-final message(s) by substituting placeholder content — an empty-content turn was in the transcript and would 400 the request ('messages must have non-empty content' / INVALID_REQUEST_BODY). Self-recovering the poisoned transcript in memory; no restart needed.", healed))
}

// Substitutes the placeholder for empty non final user and assistant rows on the wire copy
func repairEmptyNonFinalMessages(ctx context.Context, logSessionID string, messages []map[string]any) []map[string]any {
	if len(messages) < 2 {
		return messages
	}
	repaired := make([]map[string]any, 0, len(messages))
	healed := 0
	last := len(messages) - 1
	for idx, msg := range messages {
		if idx != last && msg != nil && (msg["role"] == "assistant" || msg["role"] == "user") && !state.MsgHasPayload(msg) {
			cp := shallowDict(msg)
			cp["content"] = InterruptedPlaceholder
			repaired = append(repaired, cp)
			healed++
			continue
		}
		repaired = append(repaired, msg)
	}
	if healed > 0 {
		logEmptyNonFinalHeal(ctx, logSessionID, healed)
		return repaired
	}
	return messages
}

func shallowDict(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// Drops messages whose role the API rejects
func dropInvalidRoles(messages []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, m := range messages {
		role, _ := m["role"].(string)
		if !validAPIRoles[role] {
			runAgentLog().Debug("Pre-call sanitizer: dropping message with invalid role " + pyrepr.Repr(m["role"]))
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// Removes empty or invalid tool_calls arrays from assistant rows on shallow copies
func dropEmptyToolCallsArrays(messages []map[string]any) []map[string]any {
	normalized := make([]map[string]any, 0, len(messages))
	dropped := 0
	for _, msg := range messages {
		if tcs, has := msg["tool_calls"]; msg["role"] == "assistant" && has {
			if rv := reflect.ValueOf(tcs); rv.Kind() != reflect.Slice || rv.Len() == 0 {
				cp := shallowDict(msg)
				delete(cp, "tool_calls")
				msg = cp
				dropped++
			}
		}
		normalized = append(normalized, msg)
	}
	if dropped == 0 {
		return messages
	}
	runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: dropped empty/invalid tool_calls on %d assistant message(s)", dropped))
	return normalized
}

// Coerces every tool call function name to the provider safe pattern copy on write
func repairInvalidToolCallNames(messages []map[string]any) {
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		original, _ := msg["tool_calls"].([]any)
		tcs := original
		copied := false
		for idx, tc := range original {
			var name any
			var fn map[string]any
			if d, ok := pyjson.Dict(tc); ok {
				fn, _ = pyjson.Dict(d["function"])
				if fn != nil {
					name = fn["name"]
				}
			} else if n := getToolCallNameStatic(tc); n != "" {
				name = n
			}
			coerced := provider.CoerceToolName(name, "")
			if s, ok := name.(string); ok && s == coerced {
				continue
			}
			nameText, _ := name.(string)
			runAgentLog().Warn(fmt.Sprintf("Pre-call sanitizer: repairing tool_call with invalid function.name %s -> %s (id=%s)",
				pyrepr.Repr(truncateRunes(nameText, 80)), pyrepr.Repr(coerced), getToolCallID(tc)))
			if !copied {
				tcs = slices.Clone(original)
				msg["tool_calls"] = tcs
				copied = true
			}
			if d, ok := pyjson.AsObject(tc); ok {
				newFn := pyjson.ObjectOf("name", coerced, "arguments", "{}")
				if f, isDict := pyjson.AsObject(d.Vals["function"]); isDict {
					newFn = f.Copy()
					newFn.Set("name", coerced)
				}
				cp := d.Copy()
				cp.Set("function", newFn)
				tcs[idx] = cp
				continue
			}
			args := "{}"
			if s, ok := toolCallArguments(tc).(string); ok {
				args = s
			}
			tcs[idx] = pyjson.ObjectOf("id", getToolCallID(tc), "type", "function", "function", pyjson.ObjectOf("name", coerced, "arguments", args))
		}
	}
}

// Drops tool results with a missing or blank tool_call_id
func dropResultsWithoutIDs(messages []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, m := range messages {
		if m["role"] == "tool" {
			id, _ := m["tool_call_id"].(string)
			if pystr.Strip(id) == "" {
				continue
			}
		}
		kept = append(kept, m)
	}
	if len(kept) != len(messages) {
		runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: dropped %d tool result(s) with missing/empty tool_call_id", len(messages)-len(kept)))
	}
	return kept
}

type declaredCall struct {
	tc       any
	variants map[string]bool
}

// Pairs results directly after their calls, dropping positional orphans and stubbing unanswered calls
func pairToolCallsPositionally(messages []map[string]any) []map[string]any {
	var paired []map[string]any
	declared := map[string]declaredCall{}
	var order []string
	dropped, stubs := 0, 0
	flush := func() {
		keys := slices.Clone(order)
		slices.Sort(keys)
		for _, key := range keys {
			tc := declared[key].tc
			id := state.CoalesceToolCallID(tc)
			if id == "" {
				id = key
			}
			paired = append(paired, map[string]any{"role": "tool", "name": getToolCallNameStatic(tc),
				"content": "[Result unavailable — see context summary above]", "tool_call_id": id})
			stubs++
		}
		clear(declared)
		order = nil
	}
	for _, msg := range messages {
		switch msg["role"] {
		case "assistant":
			flush()
			for _, tc := range anyRows(msg["tool_calls"]) {
				if variants := state.ToolCallIDVariants(tc); len(variants) > 0 {
					key := minKey(variants)
					if _, seen := declared[key]; !seen {
						order = append(order, key)
					}
					declared[key] = declaredCall{tc, variants}
				}
			}
		case "tool":
			resultVariants := state.ToolResultIDVariants(msg["tool_call_id"])
			matched := -1
			for i, k := range order {
				if idSetsIntersect(declared[k].variants, resultVariants) {
					matched = i
					break
				}
			}
			if matched < 0 {
				dropped++
				continue
			}
			delete(declared, order[matched])
			order = slices.Delete(order, matched, matched+1)
		case "user":
			flush()
		}
		paired = append(paired, msg)
	}
	flush()
	if dropped > 0 {
		runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: removed %d positionally orphaned tool result(s)", dropped))
	}
	if stubs > 0 {
		runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: added %d stub tool result(s) for positionally unanswered tool call(s)", stubs))
	}
	if dropped > 0 || stubs > 0 {
		return paired
	}
	return messages
}

func minKey(set map[string]bool) string {
	first, out := true, ""
	for k := range set {
		if first || k < out {
			out, first = k, false
		}
	}
	return out
}

// Collapses duplicate call ids and drops results that answer no outstanding call
func dedupeToolCallIDs(messages []map[string]any) []map[string]any {
	outstanding := map[string]int{}
	groups := map[int]map[string]bool{}
	nextGroup, removed := 0, 0
	var deduped []map[string]any
	for _, msg := range messages {
		switch msg["role"] {
		case "assistant":
			calls := anyRows(msg["tool_calls"])
			if len(calls) == 0 {
				break
			}
			var keptTCs []any
			for _, tc := range calls {
				variants := state.ToolCallIDVariants(tc)
				clash := false
				for v := range variants {
					if _, ok := outstanding[v]; ok {
						clash = true
						break
					}
				}
				if len(variants) > 0 && clash {
					removed++
					continue
				}
				if len(variants) > 0 {
					g := nextGroup
					nextGroup++
					groups[g] = variants
					for v := range variants {
						if _, ok := outstanding[v]; !ok {
							outstanding[v] = g
						}
					}
				}
				keptTCs = append(keptTCs, tc)
			}
			if len(keptTCs) > 0 {
				cp := shallowDict(msg)
				cp["tool_calls"] = keptTCs
				msg = cp
			} else if len(keptTCs) != len(calls) {
				cp := shallowDict(msg)
				delete(cp, "tool_calls")
				msg = cp
			}
		case "tool":
			resultVariants := state.ToolResultIDVariants(msg["tool_call_id"])
			candidate := -1
			for v := range resultVariants {
				if g, ok := outstanding[v]; ok && (candidate < 0 || g < candidate) {
					candidate = g
				}
			}
			if len(resultVariants) > 0 && candidate < 0 {
				removed++
				continue
			}
			if candidate >= 0 {
				for v := range groups[candidate] {
					if outstanding[v] == candidate {
						delete(outstanding, v)
					}
				}
				delete(groups, candidate)
			}
		}
		deduped = append(deduped, msg)
	}
	if removed == 0 {
		return messages
	}
	runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: removed %d duplicate tool_call_id reference(s)", removed))
	return deduped
}

// Aligns each tool result wire name with the function name of its call on a per call copy
func realignToolResultNames(messages []map[string]any) []map[string]any {
	callNames := map[string]string{}
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		for _, tc := range anyRows(msg["tool_calls"]) {
			cid, nm := pystr.Strip(getToolCallID(tc)), getToolCallNameStatic(tc)
			if cid != "" && nm != "" {
				callNames[cid] = nm
			}
		}
	}
	var realigned []string
	aligned := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if msg["role"] == "tool" {
			id, _ := msg["tool_call_id"].(string)
			expected := callNames[pystr.Strip(id)]
			current, _ := msg["name"].(string)
			if expected != "" && current != "" && current != expected {
				cp := shallowDict(msg)
				cp["name"] = expected
				msg = cp
				realigned = append(realigned, current+" -> "+expected)
			}
		}
		aligned = append(aligned, msg)
	}
	if len(realigned) == 0 {
		return messages
	}
	runAgentLog().Debug(fmt.Sprintf("Pre-call sanitizer: realigned %d tool result name(s) with their tool_call function name (%s)", len(realigned), strings.Join(realigned, ", ")))
	return aligned
}

// Fixes orphaned and malformed tool call pairs before every model call, healing empty rows first
func sanitizeAPIMessages(ctx context.Context, logSessionID string, messages []map[string]any) []map[string]any {
	messages = dropInvalidRoles(messages)
	messages = repairEmptyNonFinalMessages(ctx, logSessionID, messages)
	messages = dropEmptyToolCallsArrays(messages)
	repairInvalidToolCallNames(messages)
	messages = dropResultsWithoutIDs(messages)
	messages = pairToolCallsPositionally(messages)
	messages = dedupeToolCallIDs(messages)
	return realignToolResultNames(messages)
}

// Reconciles assistant reasoning keys with the current route, returns the number of turns changed
func (a *AIAgent) reapplyReasoningEchoForProvider(apiMessages []map[string]any) int {
	rows := make([]any, len(apiMessages))
	for i, m := range apiMessages {
		rows[i] = m
	}
	route := a.reasoningReplayRoute()
	return reapplyReasoningEcho(rows, route.Pad, route.Carriers)
}

// Clears the in flight turn marker at the turn end persist, leaving a persist disabled fork's parent slot alone
func noteTurnPersisted(a *AIAgent) {
	a.inflightTurnID = ""
	if !a.persistDisabled {
		sessionID := a.inflightTurnSessionID
		if sessionID == "" {
			sessionID = a.sessionID
		}
		if sessionID != "" {
			inflightTurnsLock.Lock()
			delete(inflightTurnsBySession, sessionID)
			inflightTurnsLock.Unlock()
		}
	}
	a.inflightTurnSessionID = ""
}
