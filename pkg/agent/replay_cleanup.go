package agent

import (
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func replayCleanupLog() *slog.Logger { return slog.Default().With("logger", "agent.replay_cleanup") }

// Orphan recovery notices, side effecting first, for an interrupted block and for a dangling tail
var (
	interruptedNotices = [2]string{
		"[Orphan recovery: interrupted side-effecting tool may have executed; its effect is UNKNOWN. Inspect state before retrying.]",
		"[Orphan recovery: interrupted read-only tool did not complete.]",
	}
	danglingNotices = [2]string{
		"[Orphan recovery: this tool may have executed before Hermes stopped; its effect is UNKNOWN. Inspect current state before retrying.]",
		"[Orphan recovery: this read-only tool did not complete and had no effect.]",
	}
)

// Bracketed marker line every executor ends a killed run's output with
var interruptMarkerLine = func() interface{ MatchString(string) (bool, error) } {
	re, err := pyre.CompileIgnoreCase(`^\[(?:command|execution) interrupted\b[^\n]*\]\s*$`)
	if err != nil {
		panic(err)
	}
	return re
}()

// True only when the executor interrupt marker is the last output line of a failed or bare text result
func IsInterruptedToolResult(content any) bool {
	text, ok := content.(string)
	if !ok {
		return false
	}
	output := text
	if strings.HasPrefix(pystr.Lstrip(text), "{") {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return false
		}
		envelope, ok := parsed.(*pyjson.Object)
		if !ok {
			return false
		}
		exitCode, _ := envelope.Get("exit_code")
		switch c := exitCode.(type) {
		case nil:
			return false
		case bool:
			if !c {
				return false
			}
		case int64:
			if c == 0 {
				return false
			}
		case float64:
			if c == 0 {
				return false
			}
		}
		out, _ := envelope.Get("output")
		if output, ok = out.(string); !ok {
			return false
		}
	}
	trimmed := pystr.Rstrip(output)
	last := trimmed[strings.LastIndex(trimmed, "\n")+1:]
	matched, _ := interruptMarkerLine.MatchString(last)
	return matched
}

func replayCallName(call map[string]any) string {
	fn, _ := pyjson.Dict(call["function"])
	if name := fn["name"]; pyrepr.Truthy(name) {
		return pyrepr.Str(name)
	}
	return ""
}

func replayCallID(call map[string]any) string {
	if id := call["id"]; pyrepr.Truthy(id) {
		return pyrepr.Str(id)
	}
	if id := call["call_id"]; pyrepr.Truthy(id) {
		return pyrepr.Str(id)
	}
	return ""
}

// The tool calls of a row as dicts
func replayCalls(v any) []map[string]any {
	list, _ := listOfAny(v)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		if m, ok := pyjson.Dict(c); ok {
			out = append(out, m)
		}
	}
	return out
}

func anySideEffecting(calls []map[string]any) bool {
	for _, c := range calls {
		if tools.ToolMayHaveSideEffect(replayCallName(c)) {
			return true
		}
	}
	return false
}

// Effect disposition and notice for an interrupted or dangling call of the named tool
func orphanRecovery(name string, notices [2]string) (string, string) {
	if tools.ToolMayHaveSideEffect(name) {
		return "unknown", notices[0]
	}
	return "none", notices[1]
}

// Content of a row, the empty string when the key is missing
func rowContent(m map[string]any) any {
	if v, ok := m["content"]; ok {
		return v
	}
	return ""
}

// Drops interrupted read only tool blocks and rewrites interrupted side effecting results as recovery notices
func StripInterruptedToolTails(history []map[string]any) []map[string]any {
	if len(history) == 0 {
		return history
	}
	cleaned := make([]map[string]any, 0, len(history))
	for i, n := 0, len(history); i < n; {
		msg := history[i]
		if _, has := msg["tool_calls"]; has && msg["role"] == "assistant" {
			j := i + 1
			for j < n && history[j]["role"] == "tool" {
				j++
			}
			results := history[i+1 : j]
			interrupted := false
			for _, r := range results {
				if IsInterruptedToolResult(rowContent(r)) {
					interrupted = true
					break
				}
			}
			if interrupted {
				calls := replayCalls(msg["tool_calls"])
				if anySideEffecting(calls) {
					names := map[string]string{}
					for _, c := range calls {
						names[replayCallID(c)] = replayCallName(c)
					}
					cleaned = append(cleaned, msg)
					for _, r := range results {
						if IsInterruptedToolResult(rowContent(r)) {
							id := ""
							if v := r["tool_call_id"]; pyrepr.Truthy(v) {
								id = pyrepr.Str(v)
							}
							disposition, content := orphanRecovery(names[id], interruptedNotices)
							rewritten := make(map[string]any, len(r)+1)
							for k, v := range r {
								rewritten[k] = v
							}
							rewritten["effect_disposition"], rewritten["content"] = disposition, content
							r = rewritten
						}
						cleaned = append(cleaned, r)
					}
				} else {
					replayCleanupLog().Debug(fmt.Sprintf("Stripping interrupted read-only assistant→tool replay block (indices %d–%d, tool_results=%d)", i, j-1, len(results)))
				}
				i = j
				continue
			}
		}
		if msg["role"] == "tool" && IsInterruptedToolResult(rowContent(msg)) {
			replayCleanupLog().Debug("Stripping orphan interrupted tool result from replay history")
		} else {
			cleaned = append(cleaned, msg)
		}
		i++
	}
	return cleaned
}

// Drops a trailing unanswered read only tool call row, answering side effecting ones as UNKNOWN effect
func StripDanglingToolCallTail(history []map[string]any) []map[string]any {
	if len(history) == 0 {
		return history
	}
	last := history[len(history)-1]
	if last == nil || last["role"] != "assistant" || !pyrepr.Truthy(last["tool_calls"]) {
		return history
	}
	calls := replayCalls(last["tool_calls"])
	if anySideEffecting(calls) {
		recovered := append([]map[string]any(nil), history...)
		for _, c := range calls {
			name := replayCallName(c)
			if name == "" {
				name = "unknown"
			}
			disposition, content := orphanRecovery(name, danglingNotices)
			recovered = append(recovered, toolResultDict(name, content, replayCallID(c), disposition))
		}
		replayCleanupLog().Warn("Recovered dangling side-effecting tool call(s) as UNKNOWN instead of erasing them")
		return recovered
	}
	replayCleanupLog().Debug(fmt.Sprintf("Stripping dangling unanswered read-only assistant(tool_calls) tail (%d call(s))", len(calls)))
	return history[:len(history)-1]
}

// Tool result row in the hermes dict shape, wire name plus top level risk and effect disposition keys
func toolResultDict(name, content, toolCallID, disposition string) map[string]any {
	d := state.MessageToDict(tools.MakeToolResultMessage(name, content, toolCallID, disposition))
	out := map[string]any{"role": d["role"], "name": name}
	for k, v := range d {
		if k != "display_metadata" {
			out[k] = v
		}
	}
	meta, _ := d["display_metadata"].(map[string]any)
	rest := map[string]any{}
	for k, v := range meta {
		switch k {
		case tools.ToolOutputRiskKey, tools.EffectDispositionKey:
			out[k] = v
		default:
			rest[k] = v
		}
	}
	if len(rest) > 0 {
		out["display_metadata"] = rest
	}
	return out
}

// Both tail strippers in canonical order, interrupted blocks then the dangling tail
func SanitizeReplayHistory(history []map[string]any) []map[string]any {
	if len(history) == 0 {
		return history
	}
	return StripDanglingToolCallTail(StripInterruptedToolTails(history))
}

// Every destructive replay transform in the shared order, nil now reading the wall clock
func CanonicalizeReplayHistory(history []map[string]any, now *float64) []map[string]any {
	if len(history) == 0 {
		return history
	}
	clock := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		clock = *now
	}
	cleaned := StripDanglingToolCallTail(StripInterruptedToolTails(history))
	return StripStaleDangerousConfirmations(cleaned, clock, DangerousConfirmationExpirySeconds)
}

// Seconds a dangerous confirmation stays replayable
const DangerousConfirmationExpirySeconds = 60.0

// Phrases that unlock destructive host actions, matched as lowercase substrings
var dangerousConfirmationPatterns = []string{
	"confirm forced restart", "confirm forced reboot", "confirm shutdown", "confirm reboot", "confirm power off",
	"yes, delete everything", "confirm wipe", "confirm factory reset",
	"確認強制重開機", "確認強制重開", "確認重啟",
}

// Text an expired dangerous confirmation is redacted to, keeping the row for role alternation
const expiredConfirmationSentinel = "[A high-risk confirmation previously given here has EXPIRED and must " +
	"not be acted on. Ask the user to re-confirm explicitly before " +
	"performing any destructive action.]"

// True when user message text holds a known dangerous confirmation phrase
func IsDangerousConfirmation(content any) bool {
	text, ok := content.(string)
	if !ok {
		return false
	}
	lowered := pystr.Lower(pystr.Strip(text))
	for _, p := range dangerousConfirmationPatterns {
		if strings.Contains(lowered, p) {
			return true
		}
	}
	return false
}

// Redacts timestamped dangerous confirmations older than the expiry, an untrustworthy stamp counting as expired
func StripStaleDangerousConfirmations(history []map[string]any, now, expirySeconds float64) []map[string]any {
	if len(history) == 0 {
		return history
	}
	cleaned := make([]map[string]any, 0, len(history))
	for _, msg := range history {
		var ts any
		if msg != nil && msg["role"] == "user" {
			ts = msg["timestamp"]
		}
		if ts == nil || !IsDangerousConfirmation(rowContent(msg)) {
			cleaned = append(cleaned, msg)
			continue
		}
		age := math.Inf(1)
		if tsF, ok := pydatetime.CoerceEpoch(ts, "", "message timestamp"); ok {
			age = now - tsF
		}
		if age >= 0 && age <= expirySeconds {
			cleaned = append(cleaned, msg)
			continue
		}
		preview, _ := msg["content"].(string)
		runes := []rune(preview)
		replayCleanupLog().Debug(fmt.Sprintf("Redacting stale dangerous-confirmation text in user message (age=%ss, expiry=%ss): %s",
			pyFixed1(age), pyFixed1(expirySeconds), pyStrRepr(string(runes[:min(len(runes), 80)]))))
		redacted := make(map[string]any, len(msg))
		for k, v := range msg {
			redacted[k] = v
		}
		redacted["content"] = expiredConfirmationSentinel
		state.DropStaleAPIContent(redacted)
		cleaned = append(cleaned, redacted)
	}
	return cleaned
}

// Python percent point one f formatting, inf and nan spelled the Python way
func pyFixed1(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	return fmt.Sprintf("%.1f", f)
}

// Canonical replay of message rows, unchanged rows keeping their pointers, zero now reading the wall clock
func canonicalizeReplayHistory(messages []*state.Message, now float64) []*state.Message {
	if len(messages) == 0 {
		return messages
	}
	dicts := make([]map[string]any, len(messages))
	origin := map[uintptr]*state.Message{}
	for i, m := range messages {
		dicts[i] = state.MessageToDict(m)
		origin[reflect.ValueOf(dicts[i]).Pointer()] = m
	}
	var clock *float64
	if now != 0 {
		clock = &now
	}
	out := CanonicalizeReplayHistory(dicts, clock)
	result := make([]*state.Message, len(out))
	for i, d := range out {
		if m, ok := origin[reflect.ValueOf(d).Pointer()]; ok {
			result[i] = m
			continue
		}
		result[i] = state.MessageFromDict(d)
	}
	return result
}
