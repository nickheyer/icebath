package tools

import (
	"context"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	failureNextStepsRetry = "Wait a minute and send /retry, or switch models with /model."
	failureNextStepsLoop  = "Your message is saved. Send `continue` to try again, or start a new session with /new. " +
		"If it happens again, run `icebath doctor` and share the error details."
)

// FailureCauseGlossTable maps a classified failure reason or site code to one plain clause naming what happened
var FailureCauseGlossTable = map[string]string{
	"timeout":                 "the AI model service did not respond in time",
	"rate_limit":              "the AI model service was rate-limited (too many requests)",
	"upstream_rate_limit":     "the AI model service was rate-limited (too many requests)",
	"overloaded":              "the AI model service is overloaded right now",
	"server_error":            "the AI model service returned an internal error",
	"billing":                 "the AI model service says the account's usage or credit limit is reached",
	"billing_unverified":      "the AI model service says the account's usage or credit limit is reached",
	"auth":                    "the AI model service rejected the sign-in",
	"auth_permanent":          "the AI model service rejected the sign-in",
	"upstream_blocked":        "a firewall/CDN in front of the AI model service blocked the request",
	"model_not_found":         "the model {subject} uses was not found at the AI model service",
	"content_policy_blocked":  "the AI model service's safety filter rejected the request",
	"provider_policy_blocked": "the AI model service refused the request because of a policy on the account",
	"data_policy_denied":      "{possessive} context holds a connected-app result that needs a model that does not train on data",
	"context_overflow":        "{possessive} request grew too large for the model",
	"payload_too_large":       "{possessive} request grew too large for the model",
}

// Plain clause for a classified failure reason with the asker substituted, false when the reason has no gloss
func FailureCauseGloss(reason any, subject, possessive string) (string, bool) {
	key := ""
	if pyrepr.Truthy(reason) {
		key = pyrepr.Str(reason)
	}
	template, ok := FailureCauseGlossTable[key]
	if !ok || template == "" {
		return "", false
	}
	return strings.NewReplacer("{subject}", subject, "{possessive}", possessive).Replace(template), true
}

var failureCodeCopy = map[string]string{
	"context_overflow": "This conversation has grown too long for {model} to read, and Icebath couldn't shrink " +
		"it enough automatically. Start a new session with /new (your history is kept), or try " +
		"/compress once more. Switching to a model with a bigger context window also works.",
	"truncated": "The model's reply was cut off before it finished (it hit its output length limit), so " +
		"Icebath didn't run the incomplete action. Nothing was changed. Send `continue`, ask for " +
		"the work in smaller steps, or raise max_tokens for this model.",
	"invalid_response": "{label} sent back an empty or broken reply {attempts} times — it is probably overloaded " +
		"or rate-limiting you. " + failureNextStepsRetry + "\n\nDetails: {detail}",
	"loop_error": "Icebath hit repeated errors and stopped this turn so it wouldn't keep retrying. " +
		failureNextStepsLoop + "\n\nDetails: {detail}",
	"interpreter_shutdown": "Icebath was shutting down and stopped this turn. Your conversation is saved — reopen " +
		"it{resume} and send your message again.",
}

var oneOffCopy = map[string]string{
	"payload_too_large": "This conversation (including attachments) has grown too large to send to {model}, and " +
		"Icebath couldn't shrink it enough automatically. Start a new session with /new (your " +
		"history is kept), or try /compress once more.",
	"compression_disabled": "This conversation is too long for {model} and automatic shrinking is turned off in " +
		"your settings (compression.enabled). Run /compress to shrink it now, /new to start " +
		"fresh, or pick a model with a bigger context window.",
	"server_context_rejection": "The model server rejected this request as too large, but this conversation is only " +
		"about {tokens:,} tokens — well under the {window:,}-token window Icebath knows for " +
		"{model} — so shrinking it would not help. Another request on the same server (for " +
		"example a background memory review from an earlier session) was probably holding its " +
		"capacity, or the server runs {model} with a smaller window than Icebath assumes. Wait a " +
		"moment and send /retry; if it keeps happening, check the server's context setting.",
	"truncated_unreported": "The model's action arrived cut off partway through, so Icebath didn't run it. Nothing was changed. The model didn't report hitting its output " +
		"limit, so this was most likely a dropped connection or a provider/router cutting the " +
		"reply short. Send /retry; if it keeps happening, ask for the work in smaller steps.",
	"stream_dropped_tool_call": "The connection to {label} kept dropping while the model was writing a large action, " +
		"so nothing was run. Check your network and send /retry; asking for the file in smaller " +
		"pieces also helps.",
	"stream_closed_tool_call": "{label} kept closing the stream before the model finished writing its action, without " +
		"reporting an error, so nothing was run. This is usually the provider or a proxy in front " +
		"of it cutting long replies short. Send /retry; asking for the work in smaller steps also helps.",
	"local_processing_error": "Icebath hit an internal error while handling the model's reply and stopped this turn. " +
		failureNextStepsLoop + "\n\nDetails: {detail}",
	"max_iterations_no_summary": "I ran out of steps for this turn ({limit} tool calls) before finishing, and couldn't " +
		"produce a summary. Send `continue` to keep going, or raise `max_iterations` in your config.",
	"nous_rate_limit": "Wait for the reset and send /retry, or switch models with /model. To avoid waits, add " +
		"a backup provider with `icebath fallback add`.",
}

// SiteCopyTable is the chat copy for every failure code and one off loop outcome a loop site renders
var SiteCopyTable = func() map[string]string {
	out := make(map[string]string, len(failureCodeCopy)+len(oneOffCopy))
	for k, v := range failureCodeCopy {
		out[k] = v
	}
	for k, v := range oneOffCopy {
		out[k] = v
	}
	return out
}()

var siteCopyFieldName = regexp.MustCompile(`\{([^{}:!.\[]*)`)

// SiteCopyError is the KeyError an unknown site copy code raises
type SiteCopyError struct{ Code string }

func (e *SiteCopyError) Error() string { return pyrepr.Repr(e.Code) }

// PyTypeName names the Python exception class an unknown code raises
func (e *SiteCopyError) PyTypeName() string { return "KeyError" }

// Chat copy for a failure code or one off outcome with every field the caller omits rendered empty
func SiteCopy(ctx context.Context, code string, fields map[string]any) (string, error) {
	template, ok := SiteCopyTable[code]
	if !ok {
		return "", &SiteCopyError{Code: code}
	}
	kwargs := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		kwargs[k] = v
	}
	if _, ok := kwargs["home"]; !ok {
		kwargs["home"] = home.Display(home.Dir(ctx))
	}
	for _, m := range siteCopyFieldName.FindAllStringSubmatch(template, -1) {
		if _, ok := kwargs[m[1]]; !ok {
			kwargs[m[1]] = ""
		}
	}
	return i18n.PyFormat(template, kwargs)
}
