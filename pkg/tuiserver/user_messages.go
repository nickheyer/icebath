package tuiserver

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type turnErrorCopy struct{ title, hint string }

var turnErrorCodeCopy = map[string]turnErrorCopy{
	"auth":                      {"The model provider rejected the API key", "Fix the key with /model, then /retry."},
	"auth_permanent":            {"The model provider rejected the API key", "Fix the key with /model, then /retry."},
	"billing":                   {"The model provider reports no credit left", "Top up the account or switch with /model."},
	"billing_unverified":        {"The model provider reports no credit left", "Top up the account or switch with /model."},
	"rate_limit":                {"The model provider is rate-limiting requests", "Wait a moment, then /retry."},
	"upstream_rate_limit":       {"The model provider is rate-limiting requests", "Wait a moment, then /retry."},
	"upstream_blocked":          {"A firewall/CDN in front of the model provider blocked the request", "Set a User-Agent via the provider's extra_headers, or switch with /model."},
	"overloaded":                {"The model provider is overloaded", "Wait a moment, then /retry."},
	"server_error":              {"The model provider had an internal error", "Wait a moment, then /retry."},
	"timeout":                   {"The model provider did not answer in time", "Try /retry; if it keeps happening, switch with /model."},
	"context_overflow":          {"The conversation is too long for this model", "Run /compress, then /retry."},
	"payload_too_large":         {"The request was too large for this model", "Run /compress, then /retry."},
	"model_not_found":           {"The model provider does not know this model", "Pick another model with /model."},
	"content_policy_blocked":    {"The model provider refused this request (content policy)", "Rephrase and send again."},
	"provider_policy_blocked":   {"The model provider refused this request (account policy)", "Switch with /model."},
	"format_error":              {"The model provider rejected the request format", "Try /retry; if it persists, switch with /model."},
	"local_runtime_unavailable": {"The local model server is not running", "Try /retry once it is up, or switch with /model."},
	"ssl_cert_verification":     {"The connection to the model provider could not be verified (TLS)", "Check the endpoint's certificate, then /retry."},
}

var turnErrorLayerCopy = map[string]turnErrorCopy{
	"auth":      {"The model provider rejected the credentials", "Fix them with /model, then /retry."},
	"billing":   {"The model provider reports no credit left", "Top up the account or switch with /model."},
	"endpoint":  {"Your custom model endpoint did not answer", "Check the endpoint is running, then /retry."},
	"streaming": {"The connection to the model provider dropped mid-reply", "Send /retry."},
	"disk":      {"The disk is full, so Icebath could not save the turn", "Free some space, then /retry."},
	"gateway":   {"Icebath hit an internal error while running this turn", "Send /retry; type /logs for the trace."},
	"provider":  {"The model provider returned an error", "Send /retry, or switch with /model."},
}

var turnErrorNoRetryHint = map[string]string{
	"local_runtime_unavailable": "Start the local model server (Icebath desktop app, or `icebath model` → Local models), or switch with /model.",
}

var turnErrorDefault = turnErrorCopy{"The request failed", "Send /retry, or switch with /model."}

const turnErrorDetailLimit = 400

// Surface field as stripped text, empty when absent or falsy
func surfaceText(surface map[string]any, key string) string {
	v := surface[key]
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Copy of a failure surface chosen by its code, then its layer, then the default
func turnErrorCopyFor(surface map[string]any) turnErrorCopy {
	if c, ok := turnErrorCodeCopy[surfaceText(surface, "code")]; ok {
		return c
	}
	if c, ok := turnErrorLayerCopy[surfaceText(surface, "layer")]; ok {
		return c
	}
	return turnErrorDefault
}

// Plain title of a failed turn with the provider in parentheses when the surface names one
func turnErrorTitle(surface map[string]any) string {
	title := turnErrorCopyFor(surface).title
	if providerName := pystr.Strip(surfaceText(surface, "provider")); providerName != "" {
		title += " (" + providerName + ")"
	}
	return title
}

// Next step for a failed turn, reworded away from a retry when it is not worth one
func turnErrorHint(surface map[string]any, recoverable bool) string {
	code := surfaceText(surface, "code")
	hint := turnErrorCopyFor(surface).hint
	if noRetry, ok := turnErrorNoRetryHint[code]; ok && (!recoverable || surface["retryable"] == false) {
		return noRetry
	}
	if recoverable {
		return hint
	}
	return strings.ReplaceAll(hint, "Send /retry", "Pick another model with /model")
}

// Assistant slot text of an unanswered turn with title, provider detail line and next step
func turnErrorText(err any, surface map[string]any, recoverable bool) string {
	raw := ""
	if pyrepr.Truthy(err) {
		raw = pyrepr.Str(err)
	}
	detail := strings.Join(pystr.SplitWhitespace(raw, -1), " ")
	if pystr.Len(detail) > turnErrorDetailLimit {
		detail = pystr.Slice(detail, 0, turnErrorDetailLimit-1) + "…"
	}
	lines := []string{turnErrorTitle(surface) + ". Your message was not answered."}
	if detail != "" {
		lines = append(lines, "Details: "+detail)
	}
	lines = append(lines, turnErrorHint(surface, recoverable))
	return strings.Join(lines, "\n")
}

// Refusal of a history mutating command while a reply streams or a compress runs
func busyMessage(command string, compressing bool) string {
	name := strings.TrimLeft(command, "/")
	if compressing {
		return "session busy — a /compress is running; run /" + name + " when it finishes."
	}
	return "session busy — Icebath is still replying. Stop the current reply first (Stop button, or Ctrl+C in a terminal), then run /" + name + "."
}

var authLockTimeoutPrefixes = []string{"Timed out waiting for auth store lock", "Timed out waiting for shared Nous auth lock"}

// Agent start failure copy, the wait for the lock holder when auth lock contention caused it
func agentInitFailedMessage(err any) string {
	detail := strings.TrimRight(pyrepr.Str(err), ". ")
	for _, prefix := range authLockTimeoutPrefixes {
		if strings.Contains(detail, prefix) {
			return "Icebath could not start the assistant for this session. Details: " + detail + ". " +
				"Wait for the other process to release the lock (or exit it — check for a running " +
				"dashboard or background icebath process), then retry."
		}
	}
	return "Icebath could not start the assistant for this session. Details: " + detail + ". " +
		"Check the model and provider with /model, or run `icebath setup` in a terminal to reconfigure."
}

const agentStillStarting = "Icebath is still starting this session (loading tools), so this command could not run yet. " +
	"Wait for the status bar to show ready and try again."

const agentBuildAbandoned = "agent build aborted: the session was closed or replaced before the build finished"

const agentMissingForTurn = "Icebath could not start the assistant for this session, so your message was not run. " +
	"Reopen the session (or start a new one with /new) and send it again."

// Resume failure copy naming the unreadable transcript
func resumeFailedMessage(err any) string {
	return "Could not reopen that session (its transcript could not be read). Details: " + pyrepr.Str(err) + ". " +
		"Start a new session (/new), or pick another from /sessions."
}
