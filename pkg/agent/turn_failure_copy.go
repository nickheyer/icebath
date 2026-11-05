package agent

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// SiteFailureCodes are the failure codes loop sites mint that are not provider verdicts
var SiteFailureCodes = map[string]bool{
	"context_overflow": true, "truncated": true, "invalid_response": true, "empty_response": true, "loop_error": true,
	"interpreter_shutdown": true, "session_busy": true, "gateway_connection": true, "host_error": true,
}

// Turn boundary notices, display kind and open tail roles shared by the core closer and the gateway writer
const (
	FailedTurnNotice            = "Your request was not processed. Send it again if you still want me to carry it out."
	PartialFailedTurnNotice     = "This turn did not complete. Some actions may already have run; verify their effects before resending."
	AutomationInterruptedNotice = "An automated delivery was interrupted and was not run again. Some actions may already have run; verify their effects."
	FailedTurnDisplayKind       = "failed_turn"
	ContentPolicyNextSteps      = provider.ContentPolicyNextSteps
)

// OpenTurnTailRoles are the durable tail roles a failed turn leaves open
var OpenTurnTailRoles = map[string]bool{"user": true, "tool": true}

// Reads a Python attribute of an object, nil for dicts whose keys getattr never sees
func pyAttr(v any, name string) any {
	switch v.(type) {
	case nil, map[string]any, state.PyObject:
		return nil
	}
	return state.ToolCallField(v, name)
}

// True for a Python dict row, either a plain map or an ordered object
func isPyDict(v any) bool {
	switch v.(type) {
	case map[string]any, state.PyObject:
		return true
	}
	return false
}

// Refusal copy for a session whose own launch API key changed or is gone, naming the failed field when known
func LaunchCredentialsCopy(ctx context.Context, err any) string {
	data := pyAttr(err, "data")
	if !isPyDict(data) {
		data = map[string]any{}
	}
	field := pyAttr(err, "field")
	if !pyrepr.Truthy(field) {
		field = state.ToolCallField(data, "field")
	}
	detail := pyAttr(err, "detail")
	if !pyrepr.Truthy(detail) {
		detail = state.ToolCallField(data, "detail")
	}
	named := ""
	if pyrepr.Truthy(field) {
		named = " (" + pyrepr.Str(field) + ": " + pyStrOr(detail, "unavailable") + ")"
	}
	text := i18n.T(ctx, "gateway.errors.launch_credentials_unavailable", "", map[string]any{"detail": named})
	if pyrepr.Truthy(field) {
		return text + "\n\nDetails: launch_credentials_unavailable" + named
	}
	return text
}

// Stamps the UI verdict fields on a terminal result in place and returns it
func StampFailure(result map[string]any, reason string, retryable bool) map[string]any {
	result["failure_reason"] = reason
	result["failure_retryable"] = retryable
	return result
}

// Failed turn display kind for a boundary row persisted before closers typed it, false for anything else
func UntypedFailedTurnDisplayKind(role, content any) (string, bool) {
	text, ok := content.(string)
	if role != "assistant" || !ok {
		return "", false
	}
	switch pystr.Strip(text) {
	case FailedTurnNotice, PartialFailedTurnNotice, AutomationInterruptedNotice:
		return FailedTurnDisplayKind, true
	}
	return "", false
}

// Boundary copy for a failed turn, partial whenever a tool may already have run
func FailedTurnNoticeFor(turnMessages any) string {
	for _, row := range pyIterRows(turnMessages) {
		if !isPyDict(row) {
			continue
		}
		role := state.ToolCallField(row, "role")
		if role == "tool" || (role == "assistant" && pyrepr.Truthy(state.ToolCallField(row, "tool_calls"))) {
			return PartialFailedTurnNotice
		}
	}
	return FailedTurnNotice
}

// Rows of a Python iterable of messages, empty for None
func pyIterRows(v any) []any {
	switch rows := v.(type) {
	case []any:
		return rows
	case []map[string]any:
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out
	case []state.PyObject:
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out
	}
	return nil
}

// Human friendly provider name for chat copy
func ProviderLabelFor(prov any) string {
	return provider.ProviderLabel(pyStrOr(prov, ""))
}

// ExitFailure is the verdict for a loop exit, advisory when FailsTurn is false
type ExitFailure struct {
	Reason    string
	Retryable bool
	FailsTurn bool
}

var exitReasonFailures = []struct {
	prefix, code         string
	retryable, failsTurn bool
}{
	{"empty_response_exhausted", "empty_response", true, false},
	{"all_retries_exhausted_no_response", string(provider.ReasonServerError), true, true},
	{"pending_tool_result", "loop_error", true, true},
	{"interpreter_shutdown", "interpreter_shutdown", false, true},
	{"local_processing_error", "loop_error", false, false},
	{"repeated_outer_errors", "loop_error", true, true},
	{"error_near_max_iterations", "loop_error", true, true},
	{"context_compression_timeout", "context_overflow", false, true},
	{"context_compression_exhausted", "context_overflow", false, true},
	{"ollama_runtime_context_too_small", "context_overflow", false, true},
	{"redirect_restart_limit_exceeded", "loop_error", true, false},
	{"rebuilt_restart_limit_exceeded", "loop_error", true, false},
}

var invalidResponseCodes = map[int64]string{
	429: string(provider.ReasonRateLimit),
	500: string(provider.ReasonServerError), 502: string(provider.ReasonServerError),
	503: string(provider.ReasonOverloaded), 529: string(provider.ReasonOverloaded),
	504: string(provider.ReasonTimeout), 524: string(provider.ReasonTimeout),
}

// Failure reason for an empty or malformed 200 body, the embedded provider error code when present
func InvalidResponseFailureReason(response any) string {
	var errValue any
	if response != nil {
		errValue = pyAttr(response, "error")
	}
	var code any
	if errValue != nil {
		code = pyAttr(errValue, "code")
	}
	if code == nil && isPyDict(errValue) {
		code = state.ToolCallField(errValue, "code")
	}
	if code == nil {
		return "invalid_response"
	}
	n, err := pyint.Of(code)
	if err != nil || !n.IsInt64() {
		return "invalid_response"
	}
	if reason, ok := invalidResponseCodes[n.Int64()]; ok {
		return reason
	}
	return "invalid_response"
}

// Failure verdict for a loop exit reason by prefix, false when the exit carries none
func ExitReasonFailure(turnExitReason any) (ExitFailure, bool) {
	reason := pyStrOr(turnExitReason, "")
	for _, row := range exitReasonFailures {
		if strings.HasPrefix(reason, row.prefix) {
			return ExitFailure{Reason: row.code, Retryable: row.retryable, FailsTurn: row.failsTurn}, true
		}
	}
	return ExitFailure{}, false
}

// True for a non failed, non interrupted iteration cap result that still carries a summary
func IsMaxIterationHandoff(result any) bool {
	if !isPyDict(result) {
		return false
	}
	get := func(key string) any { return state.ToolCallField(result, key) }
	if get("failed") == true || get("interrupted") == true {
		return false
	}
	if get("completed") != false {
		return false
	}
	reason, ok := get("turn_exit_reason").(string)
	if !ok || !strings.HasPrefix(reason, "max_iterations_reached(") {
		return false
	}
	return pystr.Strip(pyStrOr(get("final_response"), "")) != ""
}

var exhaustedLeads = map[string]string{
	string(provider.ReasonRateLimit):         "{label} rate-limited every one of {attempts} attempts",
	string(provider.ReasonUpstreamRateLimit): "{label} rate-limited every one of {attempts} attempts",
	string(provider.ReasonOverloaded):        "{label} reported it was overloaded on all {attempts} attempts",
	string(provider.ReasonServerError):       "{label} returned a server error on all {attempts} attempts",
	string(provider.ReasonTimeout):           "{label} didn't respond in time on any of {attempts} attempts",
}

const exhaustedDefaultLead = "{label} didn't answer after {attempts} attempts"

var exhaustedFirstAttemptLeads = map[string]string{
	string(provider.ReasonRateLimit):         "{label} is rate-limiting requests right now",
	string(provider.ReasonUpstreamRateLimit): "{label} is rate-limiting requests right now",
}

var nonretryableCopyTable = map[string]string{
	string(provider.ReasonModelNotFound): "Model '{model}' isn't available on {label}. Pick a different model with /model " +
		"(or `icebath model` in a terminal).{prefix_hint}",
	string(provider.ReasonFormatError): "{label} rejected this request as malformed, so the model didn't answer. Start a clean " +
		"session with /new or switch models with /model; if it keeps happening, run `icebath doctor`.",
	string(provider.ReasonRoleAlternation): "{label} requires user and assistant turns to strictly alternate and rejected this " +
		"conversation's shape. Start a clean session with /new or switch models with /model.",
	string(provider.ReasonSSLCertVerification): "Icebath couldn't verify {label}'s security certificate, so the connection was refused. " +
		"This is usually a corporate proxy or an outdated certificate store on this computer — " +
		"see the terminal or `{home}/logs/agent.log` for the exact fix, or try another provider " +
		"with /model.",
	string(provider.ReasonProviderPolicyBlocked): "{label} refused this request because of a policy on your account (its data/privacy " +
		"settings, or a block the model's upstream provider placed on the account), so the model " +
		"didn't answer and retrying won't help. Check the account with the provider, or switch " +
		"models with /model.",
	string(provider.ReasonDataPolicyDenied): "This conversation holds a result from a connected app that may only go to a model that " +
		"does not train on your data, and '{model}' on {label} can't promise that. Switch models " +
		"with /model, run /compress to summarize the result away, or start fresh with /new. If you " +
		"are on the free tier, no free model can take this content: upgrade, or use /compress or /new.",
	string(provider.ReasonUpstreamBlocked): "A firewall/CDN in front of {label} blocked the request before it reached the model, so " +
		"your key is probably fine. Set a custom User-Agent via the provider's extra_headers, check " +
		"the proxy/WAF rules, or switch providers with /model.",
}

const nonretryableDefaultCopy = "{label} rejected the request and retrying won't help. Pick another model with /model, " +
	"or check the details in `{home}/logs/agent.log`."

var authCopy = map[string]string{
	"oauth": "{label} rejected your sign-in, so the model can't be reached. Sign in again: `{relogin}`.",
	"api_key": "{label} rejected your API key, so the model can't be reached. Update it in " +
		"Settings → Providers, or run `icebath setup` in a terminal.",
}

// Chat copy once retries and fallback are exhausted, naming a known reset window or the lifetime cap
func ExhaustedCopy(reason, label string, attempts int, summary string, resetSeconds *float64, err error) string {
	if lifetime, ok := err.(*provider.StreamLifetimeExceededError); ok {
		capText, _ := pyrepr.Format(lifetime.Cap, "g")
		return "The response was still streaming when agent.stream_max_lifetime (" + capText + "s) cut it, " +
			"on all " + strconv.Itoa(attempts) + " attempts. Raise agent.stream_max_lifetime, or set it to 0 to turn the " +
			"cap off, if this model legitimately streams this long."
	}
	_, firstLead := exhaustedFirstAttemptLeads[reason]
	firstAttempt := attempts == 1 && firstLead
	leads := exhaustedLeads
	if firstAttempt {
		leads = exhaustedFirstAttemptLeads
	}
	template, ok := leads[reason]
	if !ok {
		template = exhaustedDefaultLead
	}
	lead := strings.NewReplacer("{label}", label, "{attempts}", strconv.Itoa(attempts)).Replace(template)
	situation := "it looks temporarily unavailable. Wait a minute and send /retry, or switch models with /model."
	if resetSeconds != nil && (*resetSeconds >= 120 || firstAttempt && *resetSeconds > 0) {
		window := provider.FormatResetWindow(time.Duration(math.Trunc(*resetSeconds)) * time.Second)
		situation = "its usage limit resets in " + window + ". Send /retry after that, or switch models with /model."
	}
	return lead + " — " + situation + " To avoid this in future, " +
		"add a backup provider with `icebath fallback add`.\n\nProvider said: " + summary
}

// Wall clock in seconds since the epoch, swappable by tests
var failureCopyNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// One line naming the local time a provider limit lifts and the wait remaining, empty once it has passed
func LimitResetCopy(resetsAt float64, now *float64) string {
	current := failureCopyNow()
	if now != nil {
		current = *now
	}
	remaining := int64(resetsAt - current)
	if remaining <= 0 {
		return ""
	}
	total := (remaining + 59) / 60
	hours, minutes := total/60, total%60
	wait := strconv.FormatInt(minutes, 10) + "m"
	if hours != 0 {
		wait = strconv.FormatInt(hours, 10) + "h " + pad2(minutes) + "m"
	}
	clock := time.Unix(int64(math.Floor(resetsAt)), 0).Local().Format("15:04")
	return "Limit resets at " + clock + " (in " + wait + ")."
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// Chat copy for a terminal non retryable rejection such as auth, a missing model, TLS or a generic 4xx
func NonretryableCopy(ctx context.Context, classified *provider.Classified, prov, model any, summary, prefixSuggestion string) (string, error) {
	label := ProviderLabelFor(prov)
	var template string
	if classified.IsAuth() {
		template = authCopy[provider.ProviderAuthKind(pyStrOr(prov, ""))]
	} else if t, ok := nonretryableCopyTable[string(classified.Reason)]; ok {
		template = t
	} else {
		template = nonretryableDefaultCopy
	}
	prefixHint := ""
	if prefixSuggestion != "" {
		prefixHint = " If you typed the name yourself it may be missing its vendor prefix — did you mean '" + prefixSuggestion + "'?"
	}
	body, err := i18n.PyFormat(template, map[string]any{
		"label": label, "model": model, "home": home.Display(home.Dir(ctx)), "prefix_hint": prefixHint,
		"relogin": provider.OAuthReloginCommand(pyStrOr(prov, "")),
	})
	if err != nil {
		return "", err
	}
	return body + "\n\nProvider said: " + summary, nil
}

// Chat copy for a provider safety filter refusal
func ContentPolicyCopy(label, summary string) string {
	return label + "'s safety filter refused this request, so the model didn't answer. " +
		ContentPolicyNextSteps + "\n\nProvider said: " + summary
}

// First line of an error text capped at limit characters for a trailing details line
func ShortDetail(exc any, limit int) string {
	text := pyrepr.Str(exc)
	if text == "" {
		text = pyrepr.TypeName(exc)
	}
	lines := pystr.Splitlines(pystr.Strip(text), false)
	first := pyrepr.TypeName(exc)
	if len(lines) > 0 {
		first = lines[0]
	}
	runes := pystr.Runes(first)
	if len(runes) <= limit {
		return first
	}
	end := limit - 1
	if end < 0 {
		end = max(len(runes)+end, 0)
	}
	return pystr.FromRunes(runes[:end]) + "…"
}
