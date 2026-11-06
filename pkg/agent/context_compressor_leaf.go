package agent

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	minSummaryTokens        = 2000
	leanTailFloorTokens     = 10_000
	leanTailCapTokens       = 25_000
	skillPrunedMarkerPrefix = "[SKILL_PRUNED:"
	truncatedSummaryMarker  = "finish_reason=length"
)

// One summary failure that aborts compression, its compressor flag, telemetry class and warning with the preserved count
type terminalSummaryFailure struct {
	Flag    string
	Class   string
	Message string
}

// Summary failures that abort compress regardless of abort_on_summary_failure, in precedence order
var terminalSummaryFailures = []terminalSummaryFailure{
	{"_last_summary_auth_failure", "summary_auth_failure",
		"Summary generation failed with a terminal access or quota error — aborting compression. %d " +
			"message(s) preserved unchanged; the session was NOT rotated. Check the provider credential, " +
			"permission, quota, or inference endpoint, then retry with /compress or start fresh with /new."},
	{"_last_summary_network_failure", "summary_network_failure",
		"Summary generation failed with a network/connection error — aborting compression. %d message(s) " +
			"preserved unchanged; the session was NOT rotated. This is transient: retry with /compress once " +
			"connectivity recovers, or continue the conversation as-is."},
	{"_last_summary_truncated_failure", "summary_truncated_failure",
		"Summary generation failed (output hit the token cap; summary is incomplete) — aborting compression. " +
			"%d message(s) preserved unchanged; the session was NOT rotated. A truncated summary would silently " +
			"lose context: retry with /compress, or raise the summarizer's output budget."},
	{"_last_summary_empty_content_failure", "summary_empty_content_failure",
		"Summary generation failed (LLM returned empty content) — aborting compression. %d message(s) " +
			"preserved unchanged; the session was NOT rotated. This indicates upstream provider degradation: " +
			"retry with /compress once the provider recovers, or continue the conversation as-is."},
	{"_last_summary_overload_failure", "summary_overload_failure",
		"Summary generation failed because the provider is overloaded — aborting compression. %d message(s) " +
			"preserved unchanged; the session was NOT rotated. Retry with /compress once capacity recovers, " +
			"or continue the conversation as-is."},
}

// Canonical prune marker for a skill shared by every emit and presence check
func skillPrunedMarker(skillName string) string {
	return skillPrunedMarkerPrefix + " content lost in compression; reload with skill_view(name='" + skillName + "')]"
}

// True for a bracketed chars summary under 400 code points or a lean tail demotion line
func isSummaryLine(content string) bool {
	s := withoutSkillSafetyRule(content)
	return strings.HasPrefix(s, "[") && strings.Contains(s, " chars)") && utf8.RuneCountInString(s) < 400 || isDemotedRow(s)
}

// Field of a dict or object shaped tool call, def when the key is absent
func tcGet(obj any, key string, def any) any {
	switch o := obj.(type) {
	case map[string]any:
		if v, ok := o[key]; ok {
			return v
		}
		return def
	case *pyjson.Object:
		if v, ok := o.Get(key); ok {
			return v
		}
		return def
	case nil:
		return def
	}
	if v := state.ToolCallField(obj, key); v != nil {
		return v
	}
	return def
}

// Text of a content part, the string itself or a dict text, false otherwise
func partText(item any) (any, bool) {
	switch p := item.(type) {
	case string:
		return p, true
	case map[string]any:
		return p["text"], true
	case *pyjson.Object:
		v, _ := p.Get("text")
		return v, true
	}
	return nil, false
}

// Text view of message content for substring checks, list parts joined by newlines
func contentTextForContains(content any) string {
	if parts, ok := content.([]any); ok || isPartList(content) {
		if !ok {
			parts = pyIterRows(content)
		}
		var texts []string
		for _, part := range parts {
			if t, ok := partText(part); ok {
				if s, isStr := t.(string); isStr && s != "" {
					texts = append(texts, s)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	}
	return pyrepr.Str(content)
}

func isPartList(content any) bool {
	switch content.(type) {
	case []map[string]any, []state.PyObject:
		return true
	}
	return false
}

// Copy of the message carrying content with the stale api_content sidecar dropped
func rewritten(msg map[string]any, content any) map[string]any {
	out := make(map[string]any, len(msg)+1)
	for k, v := range msg {
		out[k] = v
	}
	out["content"] = content
	state.DropStaleAPIContent(out)
	return out
}

// Label and the first 80 code points of the whitespace collapsed reason on one line
func failureSuffix(reason any, label string) string {
	preview := ""
	if pyrepr.Truthy(reason) {
		preview = strings.Join(pystr.SplitWhitespace(pyrepr.Str(reason), -1), " ")
		if rs := []rune(preview); len(rs) > 80 {
			preview = string(rs[:80])
		}
	}
	if preview != "" {
		return " " + label + ": " + preview
	}
	return " " + label
}

// FAILED suffix for a tool payload with a top level error or success false, else empty
func resultFailureSuffix(content string) string { return payloadFailureSuffix(jsonDict(content)) }

// Failure suffix for an already parsed payload
func payloadFailureSuffix(payload *pyjson.Object) string {
	errValue, _ := payload.Get("error")
	success, hasSuccess := payload.Get("success")
	if !pyrepr.Truthy(errValue) && !(hasSuccess && success == false) {
		return ""
	}
	return failureSuffix(errValue, "FAILED")
}

// Parses text as a JSON object, an empty object for empty, invalid or non object input
func jsonDict(text any) *pyjson.Object {
	var parsed any
	var err error
	switch t := text.(type) {
	case string:
		if t == "" {
			return &pyjson.Object{}
		}
		parsed, err = pyjson.Loads(t)
	case []byte:
		if len(t) == 0 {
			return &pyjson.Object{}
		}
		parsed, err = pyjson.LoadsBytes(t)
	default:
		return &pyjson.Object{}
	}
	if obj, ok := parsed.(*pyjson.Object); ok && err == nil {
		return obj
	}
	return &pyjson.Object{}
}

// Match rank of one model_thresholds key as substring length and scope, false when it does not apply
func modelThresholdKeyRank(key, model, provider string) (int, int, bool) {
	scope, substr, scoped := strings.Cut(key, ":")
	if !scoped {
		return utf8.RuneCountInString(key), 0, strings.Contains(model, key)
	}
	if pystr.Lower(pystr.Strip(scope)) == provider && strings.Contains(model, substr) {
		return utf8.RuneCountInString(substr), 1, true
	}
	return 0, 0, false
}

// Longest matching model_thresholds key wins with a provider scoped key outranking a bare one, else the default
func resolveModelThreshold(model string, modelThresholds map[string]float64, def float64, provider string) float64 {
	if len(modelThresholds) == 0 || model == "" {
		return def
	}
	provider = pystr.Lower(pystr.Strip(provider))
	best, bestLen, bestScope, found := "", 0, 0, false
	for key := range modelThresholds {
		n, scope, ok := modelThresholdKeyRank(key, model, provider)
		if !ok {
			continue
		}
		if !found || n > bestLen || n == bestLen && (scope > bestScope || scope == bestScope && key > best) {
			best, bestLen, bestScope, found = key, n, scope, true
		}
	}
	if !found {
		return def
	}
	return modelThresholds[best]
}

// Route pinned for the next summary LLM call of one attempt, a context variable so a detached stalled worker never sees it
var summaryRoutePin = pyctx.NewVar("hermes_summary_route_pin", nil)

var pinnedRouteFields = []string{"provider", "model", "base_url", "api_key", "api_mode", "timeout"}

// Pinned route that names no summary model so compress inserts its deterministic fallback summary
func DeterministicSummaryRoute() map[string]any {
	return map[string]any{"label": "deterministic fallback summary", "deterministic": true}
}

// Runs fn with the next summary LLM call pinned to route, restoring the prior pin afterwards
func PinSummaryRoute(ctx context.Context, route map[string]any, fn func() error) error {
	var value any
	if route != nil {
		value = route
	}
	token := summaryRoutePin.Set(ctx, value)
	defer func() { _ = summaryRoutePin.Reset(ctx, token) }()
	return fn()
}

// Reads and consumes the pinned summary route, nil when none is pinned
func takePinnedSummaryRoute(ctx context.Context) map[string]any {
	route, _ := summaryRoutePin.Get(ctx).(map[string]any)
	if route != nil {
		summaryRoutePin.Set(ctx, nil)
	}
	return route
}

// Consumes the pin when it is the deterministic sentinel, a real route or no pin left in place
func takeDeterministicSummaryPin(ctx context.Context) bool {
	route, _ := summaryRoutePin.Get(ctx).(map[string]any)
	if route == nil || route["deterministic"] != true {
		return false
	}
	summaryRoutePin.Set(ctx, nil)
	return true
}

// Consumes the pinned route as explicit call_llm keywords, dropping None and empty fields
func pinnedSummaryCallKwargs(ctx context.Context) map[string]any {
	route := takePinnedSummaryRoute(ctx)
	out := map[string]any{}
	for _, field := range pinnedRouteFields {
		if v, ok := route[field]; ok && v != nil && v != "" {
			out[field] = v
		}
	}
	return out
}
