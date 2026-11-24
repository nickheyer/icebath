package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

var (
	ladderReasons      = map[FailoverReason]bool{ReasonOverloaded: true, ReasonServerError: true, ReasonTimeout: true}
	sseConnPhrases     = []string{"connection lost", "connection reset", "connection closed", "connection terminated", "network error", "network connection", "terminated", "peer closed", "broken pipe", "upstream connect error"}
	zaiPolicyNotes     = map[string]string{"zai_coding_overload_long": " (Z.AI Coding overload adaptive long backoff)", "zai_coding_overload_short": " (Z.AI Coding overload short retry)"}
	nonretryableLabels = map[FailoverReason]string{
		ReasonContentPolicyBlocked: "The provider's safety filter refused this request",
		ReasonUpstreamBlocked:      "A firewall/CDN in front of the provider blocked this request",
		ReasonSSLCertVerification:  "The provider's security certificate could not be verified",
		ReasonImageTooLarge:        "Request still exceeded the provider's size limit after shrinking images",
	}
	ladderStopHints     = map[string]string{"cli": "press Esc to stop", "tui": "press Esc twice to stop", "api_server": "cancel the request to stop", "cron": ""}
	vendorResetHeaders  = []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset"}
	durationComponentRe = regexp.MustCompile(`(\d+(?:\.\d+)?)(ms|h|m|s)`)
	durationUnitSeconds = map[string]float64{"h": 3600, "m": 60, "s": 1, "ms": 0.001}
)

// ZAIPolicyNote is the status suffix naming a Z.AI Coding overload backoff policy, empty for any other policy
func ZAIPolicyNote(policy string) string { return zaiPolicyNotes[policy] }

// NonretryableLabel names a non retryable reason in the fallback status line, false for an unlabelled reason
func NonretryableLabel(reason FailoverReason) (string, bool) {
	l, ok := nonretryableLabels[reason]
	return l, ok
}

// LadderReason reports a transient transport verdict the post exhaustion recovery ladder may wait out
func LadderReason(reason FailoverReason) bool { return ladderReasons[reason] }

// LadderStopHint names how a platform stops a ladder wait, the send stop hint for an unlisted platform
func LadderStopHint(platform string) string {
	if hint, ok := ladderStopHints[strings.ToLower(platform)]; ok {
		return hint
	}
	return ladderDefaultStopHint
}

const ladderDefaultStopHint = "send /stop to cancel"

// Next step copy for a content policy block
const ContentPolicyNextSteps = "Try rewording your message or removing sensitive attachments, or switch to another model with /model."

func errorHeaders(err error) http.Header {
	var hc HeaderCarrier
	if errors.As(err, &hc) {
		return hc.ResponseHeaders()
	}
	return nil
}

func durationStringSeconds(text string) (float64, bool) {
	raw := strings.ToLower(strings.TrimSpace(text))
	if raw == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, true
	}
	parts := durationComponentRe.FindAllStringSubmatch(raw, -1)
	if len(parts) == 0 {
		return 0, false
	}
	var rebuilt strings.Builder
	total := 0.0
	for _, p := range parts {
		rebuilt.WriteString(p[1] + p[2])
		total += atof(p[1]) * durationUnitSeconds[p[2]]
	}
	if rebuilt.String() != raw {
		return 0, false
	}
	return total, true
}

func setResetFromRetryAfter(ctx map[string]any, value any, now time.Time) {
	if _, has := ctx["reset_at"]; has {
		return
	}
	if d, ok := retryAfterValue(value); ok {
		ctx["reset_at"] = float64(now.UnixNano())/1e9 + d.Seconds()
	}
}

func setResetFromVendorHeaders(ctx map[string]any, lowered map[string]string, now time.Time) {
	for _, name := range vendorResetHeaders {
		value := strings.TrimSpace(lowered[name])
		if value == "" {
			continue
		}
		seconds, ok := durationStringSeconds(value)
		if !ok {
			if absolute, okAbs := ParseAbsoluteTimestamp(value); okAbs {
				seconds, ok = absolute-float64(now.UnixNano())/1e9, true
			}
		}
		if ok && seconds > 0 {
			ctx["reset_at"] = float64(now.UnixNano())/1e9 + seconds
			return
		}
	}
}

// Value of the first key whose value is truthy, else the value of the last key, Python's a or b or c
func firstTruthyField(m map[string]any, keys ...string) any {
	var v any
	for _, k := range keys {
		if v = m[k]; pyTruthy(v) {
			return v
		}
	}
	return v
}

// Float of a Python int or float value, bools counting as ints
func pyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// ExtractAPIErrorContext pulls reason, message, and reset_at out of a provider error body, headers, and text
func ExtractAPIErrorContext(err error) map[string]any {
	ctx := map[string]any{}
	if err == nil {
		return ctx
	}
	now := time.Now()
	var apiErr *APIError
	var sdkErr *AuxSDKError
	var streamErr *ProviderStreamError
	var body map[string]any
	switch {
	case errors.As(err, &apiErr):
		body = decodeJSONObject(apiErr.Body)
	case errors.As(err, &sdkErr):
		body, _ = sdkErr.Body.(map[string]any)
	case errors.As(err, &streamErr):
		body = streamErr.Body
	}
	payload := body
	if nested, ok := body["error"].(map[string]any); ok {
		payload = nested
	}
	if payload != nil {
		if s, ok := firstTruthyField(payload, "code", "type", "error").(string); ok && strings.TrimSpace(s) != "" {
			ctx["reason"] = strings.TrimSpace(s)
		}
		message := firstTruthyField(payload, "message", "error_description")
		if s, ok := payload["error"].(string); ok && !pyTruthy(message) {
			message = s
		}
		if s, ok := message.(string); ok && strings.TrimSpace(s) != "" {
			ctx["message"] = strings.TrimSpace(s)
		}
		for _, key := range []string{"resets_at", "reset_at"} {
			if v, ok := payload[key]; ok && v != nil && v != "" {
				ctx["reset_at"] = v
				break
			}
		}
		if _, has := ctx["reset_at"]; !has {
			if secs, ok := pyNumber(payload["resets_in_seconds"]); ok {
				ctx["reset_at"] = float64(now.UnixNano())/1e9 + secs
			}
		}
		setResetFromRetryAfter(ctx, payload["retry_after"], now)
	}
	if h := errorHeaders(err); len(h) > 0 {
		lowered := LowerHeaders(h)
		if ra := lowered["retry-after"]; ra != "" {
			setResetFromRetryAfter(ctx, ra, now)
		}
		if reset := lowered["x-ratelimit-reset"]; reset != "" {
			if _, has := ctx["reset_at"]; !has {
				ctx["reset_at"] = reset
			}
		}
		if _, has := ctx["reset_at"]; !has {
			setResetFromVendorHeaders(ctx, lowered, now)
		}
	}
	if _, has := ctx["message"]; !has {
		if text := strings.TrimSpace(rawErrorText(err)); text != "" {
			ctx["message"] = truncate(text, 500)
		}
	}
	if _, has := ctx["reset_at"]; !has {
		if msg, _ := ctx["message"].(string); msg != "" {
			if delay, ok := ResetDelayFromMessage(msg); ok {
				ctx["reset_at"] = float64(now.UnixNano())/1e9 + delay.Seconds()
			}
		}
	}
	return ctx
}

// RetrySession holds the latches that outlive one API call block
type RetrySession struct {
	DisableStreaming         bool
	StreamOptionsUnsupported bool
	stream5xxProbeAt         time.Time
	stream5xxProbed          bool
	staleStreak              atomic.Int64
}

func rejectsStreamOptions(apiErr *APIError) bool {
	if apiErr == nil || (apiErr.StatusCode != 400 && apiErr.StatusCode != 422) {
		return false
	}
	body := strings.ToLower(apiErr.Body + " " + apiErr.Message)
	if !strings.Contains(body, "stream_options") {
		return false
	}
	return anyIn(body, []string{"extra", "not supported", "unrecognized", "unexpected", "unknown"})
}

func isStreamUnavailableError(err error) bool {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "stream") && strings.Contains(lower, "not supported") {
		return true
	}
	if IsStreamEventOrderError(err) {
		return true
	}
	return strings.Contains(lower, "invokemodelwithresponsestream") && (strings.Contains(lower, "accessdenied") || strings.Contains(lower, "not authorized"))
}

func streamsLlamaCpp(route Route) bool {
	return IsLlamaCppAlias(route.Provider)
}

func orUnknown2(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// IsLocalValidationError reports a local bug rather than a provider, transport, or stream failure
func IsLocalValidationError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode > 0 || apiErr.Kind != ErrUnknown {
			return false
		}
		if apiErr.Err == nil {
			return false
		}
		err = apiErr.Err
	}
	var syntaxErr *json.SyntaxError
	var empty *EmptyStreamError
	var urlErr *url.Error
	if errors.As(err, &syntaxErr) || errors.As(err, &empty) || errors.As(err, &urlErr) {
		return false
	}
	if isTransportError(err) || isCertificateError(err) || IsProviderStreamParseError(err) || TimeoutPhase(err) != "" {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var local *LocalValidationError
	if errors.As(err, &local) {
		return true
	}
	if chain := errorClassChain(err); chain != nil {
		if !slices.Contains(chain, "ValueError") && !slices.Contains(chain, "TypeError") {
			return false
		}
		if slices.Contains(chain, "JSONDecodeError") || slices.Contains(chain, "UnicodeEncodeError") || slices.Contains(chain, "SSLError") {
			return false
		}
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "nonetype") && strings.Contains(text, "not iterable") {
		return false
	}
	return true
}

// Class chain a failure presents to the classifiers: the chain a plugin host raise reports, else its kind followed by the value or type error family the kind belongs to; nil when the chain holds plain errors only
func errorClassChain(err error) []string {
	var chained interface{ ClassChain() []string }
	if errors.As(err, &chained) {
		return chained.ClassChain()
	}
	kind := errorutil.Kind(err)
	if kind == "" || kind == "error" {
		return nil
	}
	if chain, ok := errorKindChains[kind]; ok {
		return chain
	}
	return []string{kind}
}

// Kinds of the value and type error families with the families they belong to, the Go conversion and JSON encoding errors among them
var errorKindChains = map[string][]string{
	"ValueError":               {"ValueError"},
	"TypeError":                {"TypeError"},
	"UnicodeError":             {"UnicodeError", "ValueError"},
	"UnicodeDecodeError":       {"UnicodeDecodeError", "UnicodeError", "ValueError"},
	"UnicodeEncodeError":       {"UnicodeEncodeError", "UnicodeError", "ValueError"},
	"UnicodeTranslateError":    {"UnicodeTranslateError", "UnicodeError", "ValueError"},
	"JSONDecodeError":          {"JSONDecodeError", "ValueError"},
	"SSLCertVerificationError": {"SSLCertVerificationError", "SSLError", "OSError", "ValueError"},
	"NumError":                 {"NumError", "ValueError"},
	"UnsupportedValueError":    {"UnsupportedValueError", "ValueError"},
	"UnmarshalTypeError":       {"UnmarshalTypeError", "TypeError"},
	"UnsupportedTypeError":     {"UnsupportedTypeError", "TypeError"},
	"InvalidUnmarshalError":    {"InvalidUnmarshalError", "TypeError"},
}

// LocalValidationError marks a request the adapter refused before any transport attempt
type LocalValidationError struct {
	Message string
	Err     error
}

func (e *LocalValidationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "local validation error"
}

func (e *LocalValidationError) Unwrap() error { return e.Err }

func eagerFallbackStatus(cl *Classified, isUpstream, isTransport bool) string {
	if isUpstream {
		name, _ := cl.Context["upstream_provider"].(string)
		if name == "" {
			name = "aggregator"
		}
		return fmt.Sprintf("⚠️ Upstream %s rate-limited — switching to fallback model...", name)
	}
	if cl.Reason == ReasonBilling {
		if cl.BillingUnverified() {
			return "⚠️ Provider reported usage/credit exhaustion (unverified — may be a content-filter rejection) — switching to fallback provider..."
		}
		return "⚠️ Billing or credits exhausted — switching to fallback provider..."
	}
	if isTransport {
		return "⚠️ Provider unreachable — switching to fallback provider..."
	}
	return "⚠️ Rate limited — switching to fallback provider..."
}

func EagerFallbackStatus(cl *Classified, isUpstream, isTransport bool) string {
	return eagerFallbackStatus(cl, isUpstream, isTransport)
}

func formatDurationCompact(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%.0fm", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		remainingMin := int(minutes) % 60
		if remainingMin > 0 {
			return fmt.Sprintf("%dh %dm", int(hours), remainingMin)
		}
		return fmt.Sprintf("%dh", int(hours))
	}
	return fmt.Sprintf("%.1fd", hours/24)
}

// ResetHint renders ~13m until the reset_at of an error, empty without a usable future reset
func ResetHint(err error, now time.Time) string {
	resetAt, ok := ExtractAPIErrorContext(err)["reset_at"]
	if !ok {
		return ""
	}
	epoch, _ := ParseAbsoluteTimestamp(resetAt)
	remaining := epoch - float64(now.UnixNano())/1e9
	if remaining < 1 {
		return ""
	}
	return "~" + formatDurationCompact(remaining)
}
