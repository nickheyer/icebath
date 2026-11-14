package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	FailedTurnNotice = "Your request was not processed. Send it again if you still want me to carry it out."

	PartialFailedTurnNotice = "This turn did not complete. Some actions may already have run; verify their effects " +
		"before resending."

	AutomationInterruptedNotice = "An automated delivery was interrupted and was not run again. Some " +
		"actions may already have run; verify their effects."

	FailedTurnDisplayKind = "failed_turn"

	ContentPolicyNextSteps = "Try rewording your message or removing sensitive attachments, or switch to another " +
		"model with /model."

	HeldTransformFailedReply = "This reply was withheld: the plugin that rewrites replies in this session failed."

	nextStepsRetry = "Wait a minute and send /retry, or switch models with /model."
	nextStepsLoop  = "Your message is saved. Send `continue` to try again, or start a new session with /new. " +
		"If it happens again, run `icebath doctor` and share the error details."
)

func OpenTurnTailRole(role wire.Role) bool {
	return role == wire.RoleUser || role == wire.RoleTool
}

func UntypedFailedTurnDisplayKind(role, content string) (string, bool) {
	if role != string(wire.RoleAssistant) {
		return "", false
	}
	switch trimText(content) {
	case FailedTurnNotice, PartialFailedTurnNotice, AutomationInterruptedNotice:
		return FailedTurnDisplayKind, true
	}
	return "", false
}

func FailedTurnNoticeFor(turn []*Message) string {
	for _, m := range turn {
		if m == nil {
			continue
		}
		if m.Role == wire.RoleTool || (m.Role == wire.RoleAssistant && len(m.ToolCalls) > 0) {
			return PartialFailedTurnNotice
		}
	}
	return FailedTurnNotice
}

func LaunchCredentialsCopy(ctx context.Context, text Catalog, field, detail string) (string, error) {
	if text == nil {
		return "", &tool.UnfilledError{Name: "text catalog"}
	}
	named := ""
	if field != "" {
		if detail == "" {
			detail = "unavailable"
		}
		named = " (" + field + ": " + detail + ")"
	}
	body := text.T(ctx, "gateway.errors.launch_credentials_unavailable", map[string]any{"detail": named})
	if field == "" {
		return body, nil
	}
	return body + "\n\nDetails: launch_credentials_unavailable" + named, nil
}

var copyField = regexp.MustCompile(`\{[a-z_]+\}`)

func fillCopy(template string, fields map[string]string) string {
	return copyField.ReplaceAllStringFunc(template, func(name string) string {
		return fields[name[1:len(name)-1]]
	})
}

var exhaustedLeads = map[string]string{
	model.ClassRateLimit.String():         "{label} rate-limited every one of {attempts} attempts",
	model.ClassUpstreamRateLimit.String(): "{label} rate-limited every one of {attempts} attempts",
	model.ClassOverloaded.String():        "{label} reported it was overloaded on all {attempts} attempts",
	model.ClassServerError.String():       "{label} returned a server error on all {attempts} attempts",
	model.ClassTimeout.String():           "{label} didn't respond in time on any of {attempts} attempts",
}

const exhaustedDefaultLead = "{label} didn't answer after {attempts} attempts"

var exhaustedFirstAttemptLeads = map[string]string{
	model.ClassRateLimit.String():         "{label} is rate-limiting requests right now",
	model.ClassUpstreamRateLimit.String(): "{label} is rate-limiting requests right now",
}

var nonretryableCopies = map[string]string{
	model.ClassModelNotFound.String(): "Model '{model}' isn't available on {label}. Pick a different model with /model " +
		"(or `icebath model` in a terminal).{prefix_hint}",
	model.ClassFormatError.String(): "{label} rejected this request as malformed, so the model didn't answer. Start a clean " +
		"session with /new or switch models with /model; if it keeps happening, run `icebath doctor`.",
	model.ClassRoleAlternation.String(): "{label} requires user and assistant turns to strictly alternate and rejected this " +
		"conversation's shape. Start a clean session with /new or switch models with /model.",
	model.ClassSSLCertVerification.String(): "icebath couldn't verify {label}'s security certificate, so the connection was refused. " +
		"This is usually a corporate proxy or an outdated certificate store on this computer — " +
		"see the terminal or `{home}/logs/agent.log` for the exact fix, or try another provider " +
		"with /model.",
	model.ClassProviderPolicyBlocked.String(): "{label} refused this request because of a policy on your account (its data/privacy " +
		"settings, or a block the model's upstream provider placed on the account), so the model " +
		"didn't answer and retrying won't help. Check the account with the provider, or switch " +
		"models with /model.",
	model.ClassDataPolicyDenied.String(): "This conversation holds a result from a connected app that may only go to a model that " +
		"does not train on your data, and '{model}' on {label} can't promise that. Switch models " +
		"with /model, run /compress to summarize the result away, or start fresh with /new. If you " +
		"are on the free tier, no free model can take this content: upgrade, or use /compress or /new.",
	model.ClassUpstreamBlocked.String(): "A firewall/CDN in front of {label} blocked the request before it reached the model, so " +
		"your key is probably fine. Set a custom User-Agent via the provider's extra_headers, check " +
		"the proxy/WAF rules, or switch providers with /model.",
}

const nonretryableDefaultCopy = "{label} rejected the request and retrying won't help. Pick another model with /model, " +
	"or check the details in `{home}/logs/agent.log`."

var authCopies = map[string]string{
	"oauth": "{label} rejected your sign-in, so the model can't be reached. Sign in again: `{relogin}`.",
	"api_key": "{label} rejected your API key, so the model can't be reached. Update it in " +
		"Settings → Providers, or run `icebath setup` in a terminal.",
}

const billingUnverifiedCode = "billing_unverified"

var failureCauseGloss = map[string]string{
	model.ClassTimeout.String():               "the AI model service did not respond in time",
	model.ClassRateLimit.String():             "the AI model service was rate-limited (too many requests)",
	model.ClassUpstreamRateLimit.String():     "the AI model service was rate-limited (too many requests)",
	model.ClassOverloaded.String():            "the AI model service is overloaded right now",
	model.ClassServerError.String():           "the AI model service returned an internal error",
	model.ClassBilling.String():               "the AI model service says the account's usage or credit limit is reached",
	billingUnverifiedCode:                     "the AI model service says the account's usage or credit limit is reached",
	model.ClassAuth.String():                  "the AI model service rejected the sign-in",
	model.ClassAuthPermanent.String():         "the AI model service rejected the sign-in",
	model.ClassUpstreamBlocked.String():       "a firewall/CDN in front of the AI model service blocked the request",
	model.ClassModelNotFound.String():         "the model {subject} uses was not found at the AI model service",
	model.ClassContentPolicyBlocked.String():  "the AI model service's safety filter rejected the request",
	model.ClassProviderPolicyBlocked.String(): "the AI model service refused the request because of a policy on the account",
	model.ClassDataPolicyDenied.String():      "{possessive} context holds a connected-app result that needs a model that does not train on data",
	FailureContextOverflow:                    "{possessive} request grew too large for the model",
	model.ClassPayloadTooLarge.String():       "{possessive} request grew too large for the model",
}

func FailureCauseGloss(reason, subject, possessive string) (string, bool) {
	template, ok := failureCauseGloss[reason]
	if !ok {
		return "", false
	}
	return fillCopy(template, map[string]string{"subject": subject, "possessive": possessive}), true
}

const (
	CopyPayloadTooLarge         = "payload_too_large"
	CopyCompressionDisabled     = "compression_disabled"
	CopyServerContextRejection  = "server_context_rejection"
	CopyTruncatedUnreported     = "truncated_unreported"
	CopyStreamDroppedToolCall   = "stream_dropped_tool_call"
	CopyStreamClosedToolCall    = "stream_closed_tool_call"
	CopyLocalProcessingError    = "local_processing_error"
	CopyMaxIterationsNoSummary  = "max_iterations_no_summary"
	CopyPortalRateLimit         = "portal_rate_limit"
	copyFallbackAdd             = "`icebath fallback add`"
	streamLifetimeExhaustedCopy = "The response was still streaming when agent.stream_max_lifetime (%ss) cut it, " +
		"on all %d attempts. Raise agent.stream_max_lifetime, or set it to 0 to turn the " +
		"cap off, if this model legitimately streams this long."
)

var siteCopies = map[string]string{
	FailureContextOverflow: "This conversation has grown too long for {model} to read, and icebath couldn't shrink " +
		"it enough automatically. Start a new session with /new (your history is kept), or try " +
		"/compress once more. Switching to a model with a bigger context window also works.",
	FailureTruncated: "The model's reply was cut off before it finished (it hit its output length limit), so " +
		"icebath didn't run the incomplete action. Nothing was changed. Send `continue`, ask for " +
		"the work in smaller steps, or raise max_tokens for this model.",
	FailureInvalidResponse: "{label} sent back an empty or broken reply {attempts} times — it is probably overloaded " +
		"or rate-limiting you. " + nextStepsRetry + "\n\nDetails: {detail}",
	FailureLoopError: "icebath hit repeated errors and stopped this turn so it wouldn't keep retrying. " +
		nextStepsLoop + "\n\nDetails: {detail}",
	FailureShutdown: "icebath was shutting down and stopped this turn. Your conversation is saved — reopen " +
		"it{resume} and send your message again.",
	CopyPayloadTooLarge: "This conversation (including attachments) has grown too large to send to {model}, and " +
		"icebath couldn't shrink it enough automatically. Start a new session with /new (your " +
		"history is kept), or try /compress once more.",
	CopyCompressionDisabled: "This conversation is too long for {model} and automatic shrinking is turned off in " +
		"your settings (compression.enabled). Run /compress to shrink it now, /new to start " +
		"fresh, or pick a model with a bigger context window.",
	CopyServerContextRejection: "The model server rejected this request as too large, but this conversation is only " +
		"about {tokens} tokens — well under the {window}-token window icebath knows for " +
		"{model} — so shrinking it would not help. Another request on the same server (for " +
		"example a background memory review from an earlier session) was probably holding its " +
		"capacity, or the server runs {model} with a smaller window than icebath assumes. Wait a " +
		"moment and send /retry; if it keeps happening, check the server's context setting.",
	CopyTruncatedUnreported: "The model's action arrived cut off partway through, so icebath didn't run it. Nothing was changed. The model didn't report hitting its output " +
		"limit, so this was most likely a dropped connection or a provider/router cutting the " +
		"reply short. Send /retry; if it keeps happening, ask for the work in smaller steps.",
	CopyStreamDroppedToolCall: "The connection to {label} kept dropping while the model was writing a large action, " +
		"so nothing was run. Check your network and send /retry; asking for the file in smaller " +
		"pieces also helps.",
	CopyStreamClosedToolCall: "{label} kept closing the stream before the model finished writing its action, without " +
		"reporting an error, so nothing was run. This is usually the provider or a proxy in front " +
		"of it cutting long replies short. Send /retry; asking for the work in smaller steps also helps.",
	CopyLocalProcessingError: "icebath hit an internal error while handling the model's reply and stopped this turn. " +
		nextStepsLoop + "\n\nDetails: {detail}",
	CopyMaxIterationsNoSummary: "I ran out of steps for this turn ({limit} tool calls) before finishing, and couldn't " +
		"produce a summary. Send `continue` to keep going, or raise `max_iterations` in your config.",
	CopyPortalRateLimit: "Wait for the reset and send /retry, or switch models with /model. To avoid waits, add " +
		"a backup provider with " + copyFallbackAdd + ".",
}

type CopyFields struct {
	Model    string
	Label    string
	Detail   string
	Resume   string
	Attempts int
	Limit    int
	Tokens   int
	Window   int
}

func (f CopyFields) values() map[string]string {
	return map[string]string{
		"model":    f.Model,
		"label":    f.Label,
		"detail":   f.Detail,
		"resume":   f.Resume,
		"attempts": strconv.Itoa(f.Attempts),
		"limit":    strconv.Itoa(f.Limit),
		"tokens":   groupThousands(f.Tokens),
		"window":   groupThousands(f.Window),
	}
}

func SiteCopy(code string, f CopyFields) (string, bool) {
	template, ok := siteCopies[code]
	if !ok {
		return "", false
	}
	return fillCopy(template, f.values()), true
}

func formatGeneral(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func ExhaustedCopy(reason, label string, attempts int, summary string, reset *time.Duration, cause error) string {
	var lifetime *model.StreamLifetimeError
	if errors.As(cause, &lifetime) {
		return fmt.Sprintf(streamLifetimeExhaustedCopy, formatGeneral(lifetime.Cap.Seconds()), attempts)
	}
	_, firstLead := exhaustedFirstAttemptLeads[reason]
	firstAttempt := attempts == 1 && firstLead
	table := exhaustedLeads
	if firstAttempt {
		table = exhaustedFirstAttemptLeads
	}
	template, ok := table[reason]
	if !ok {
		template = exhaustedDefaultLead
	}
	lead := fillCopy(template, map[string]string{"label": label, "attempts": strconv.Itoa(attempts)})
	situation := "it looks temporarily unavailable. " + nextStepsRetry
	if reset != nil && (*reset >= 120*time.Second || (firstAttempt && *reset > 0)) {
		situation = "its usage limit resets in " + model.FormatResetWindow(*reset) + ". " +
			"Send /retry after that, or switch models with /model."
	}
	return lead + " — " + situation + " To avoid this in future, " +
		"add a backup provider with " + copyFallbackAdd + ".\n\nProvider said: " + summary
}

func LimitResetCopy(resetsAt float64, now time.Time) string {
	remaining := int(math.Trunc(resetsAt - epochSeconds(now)))
	if remaining <= 0 {
		return ""
	}
	total := (remaining + 59) / 60
	hours, minutes := total/60, total%60
	wait := strconv.Itoa(minutes) + "m"
	if hours > 0 {
		wait = fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	whole, frac := math.Modf(resetsAt)
	at := time.Unix(int64(whole), int64(frac*1e9)).Local()
	return "Limit resets at " + at.Format("15:04") + " (in " + wait + ")."
}

func OAuthReloginCommand(provider, selector string) string {
	slug := strings.ToLower(trimText(provider))
	if slug == tool.ManagedProvider {
		return "icebath " + selector + "portal"
	}
	return "icebath " + selector + "auth add " + slug + " --type oauth"
}

func ReloginCommandHint(provider, authKind, selector string) string {
	slug := strings.ToLower(trimText(provider))
	if slug == "" {
		return "icebath " + selector + "auth add <provider>"
	}
	if authKind == "oauth" {
		return OAuthReloginCommand(slug, selector)
	}
	return "icebath " + selector + "auth add " + slug
}

type NonretryableFacts struct {
	Reason           string
	IsAuth           bool
	AuthKind         string
	Provider         string
	Label            string
	Model            string
	Summary          string
	PrefixSuggestion string
	Home             string
	Selector         string
}

func NonretryableCopy(f NonretryableFacts) string {
	template, ok := nonretryableCopies[f.Reason]
	if !ok {
		template = nonretryableDefaultCopy
	}
	if f.IsAuth {
		template = authCopies[f.AuthKind]
	}
	prefixHint := ""
	if f.PrefixSuggestion != "" {
		prefixHint = " If you typed the name yourself it may be missing its vendor prefix — did you mean " +
			"'" + f.PrefixSuggestion + "'?"
	}
	body := fillCopy(template, map[string]string{
		"label":       f.Label,
		"model":       f.Model,
		"home":        f.Home,
		"prefix_hint": prefixHint,
		"relogin":     OAuthReloginCommand(f.Provider, f.Selector),
	})
	return body + "\n\nProvider said: " + f.Summary
}

func ContentPolicyCopy(label, summary string) string {
	return label + "'s safety filter refused this request, so the model didn't answer. " +
		ContentPolicyNextSteps + "\n\nProvider said: " + summary
}

func ShortDetail(err error, limit int) string {
	text := ""
	if err != nil {
		text = err.Error()
	}
	if text == "" {
		text = fmt.Sprintf("%T", err)
	}
	first := fmt.Sprintf("%T", err)
	if lines := strings.FieldsFunc(trimText(text), func(r rune) bool { return lineBreak(r) }); len(lines) > 0 {
		first = lines[0]
	}
	runes := []rune(first)
	if len(runes) <= limit {
		return first
	}
	return string(runes[:limit-1]) + "…"
}

func lineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}
