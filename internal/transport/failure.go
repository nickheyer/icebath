package transport

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/httpx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest error body read from a refused request
const ErrorBodyLimit = 1 << 20

// Phases of a request a timeout can stop it in
const (
	PhaseConnect = "connect"
	PhaseRead    = "read"
)

// Request that ran out of time in a phase
type TimeoutError struct {
	Phase string
	Err   error
}

func (e *TimeoutError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "The request took too long."
}

func (e *TimeoutError) Unwrap() error { return e.Err }

func (e *TimeoutError) Timeout() bool { return true }

// Error a provider put inside its stream, or as the reply's text, with the body it gave
type StreamError struct {
	Status int
	Body   map[string]any
	Text   string
	Header http.Header
}

func (e *StreamError) Error() string {
	object, _ := e.Body["error"].(map[string]any)
	text := "The provider sent an error in its reply"
	if e.Status != 0 {
		text += fmt.Sprintf(", with HTTP status %d", e.Status)
	}
	if code := scalar(object["code"]); code != "" {
		text += ", code " + code
	}
	if message := scalar(object["message"]); message != "" {
		text += ": " + message
	}
	return text
}

// Codes of the errors a stream frame that wasn't JSON, or carried nothing, stands for
const (
	CodeNotJSON    = "provider_stream_non_json_data"
	CodeEmptyFrame = "provider_stream_empty_frame"
)

// Longest provider text copied into an error
const streamTextLimit = 4096

// Error of a stream frame the decoder refused, or one that carried nothing
func FrameError(raw string, header http.Header) *StreamError {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &StreamError{Body: ErrorBody(map[string]any{"code": CodeEmptyFrame,
			"message": "The provider's reply held an empty piece with nothing in it."}, 0), Header: header}
	}
	safe := redact.Text(strings.ToValidUTF8(raw, "�"), false, redact.Ends)
	if len(safe) > streamTextLimit {
		safe = safe[:streamTextLimit]
	}
	return &StreamError{Body: ErrorBody(map[string]any{"code": CodeNotJSON, "message": safe}, 0), Text: safe, Header: header}
}

// Finish reasons a relay ends with when the reply's text is an error
var errorFinishes = []string{"error", "error_finish"}

// Error a relay sent as the reply's text, nil when the reply is no error
func TextError(text, finish string, header http.Header) *StreamError {
	if text == "" || !containsFold(errorFinishes, finish) {
		return nil
	}
	made := func(payload map[string]any, status int) *StreamError {
		return &StreamError{Status: status, Body: ErrorBody(payload, status), Text: text, Header: header}
	}
	for _, frame := range ParseEvents(text) {
		payload := object(frame.Data)
		status := frame.Status
		if status == 0 {
			status = PayloadStatus(payload)
		}
		if status >= 400 || strings.EqualFold(strings.TrimSpace(frame.Event), "error") || errorShaped(payload) {
			if payload == nil {
				payload = map[string]any{}
			}
			return made(payload, status)
		}
	}
	if payload := object(text); payload != nil {
		return made(payload, PayloadStatus(payload))
	}
	if strings.TrimSpace(text) != "" {
		return made(map[string]any{}, 0)
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, item := range list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}

// Error payload in OpenAI's shape, with an error object holding its message and code
func ErrorBody(payload map[string]any, status int) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	} else if _, ok := payload["error"].(map[string]any); ok {
		return payload
	}
	code := first(payload["code"], payload["error_code"], payload["type"])
	if code == "" {
		code = "provider_stream_error"
		if status != 0 {
			code = fmt.Sprintf("HTTP_%d", status)
		}
	}
	message := first(payload["message"], payload["error_description"], payload["error"])
	message = cmp.Or(message, "The provider sent an error in its reply.")
	normalized := map[string]any{"message": message, "code": code}
	for _, key := range []string{"request_id", "param", "type"} {
		if v, ok := payload[key]; ok && v != nil && v != "" {
			normalized[key] = v
		}
	}
	return map[string]any{"error": normalized}
}

// HTTP status a payload or its error object names in its status fields
func PayloadStatus(payload map[string]any) int {
	if payload == nil {
		return 0
	}
	candidates := []any{payload["status_code"], payload["status"], payload["http_status"]}
	if inner, ok := payload["error"].(map[string]any); ok {
		candidates = append(candidates, inner["status_code"], inner["status"], inner["http_status"], inner["code"])
	}
	for _, candidate := range append(candidates, payload["code"]) {
		if code := StatusOf(candidate); candidate != nil && code != 0 {
			return code
		}
	}
	return 0
}

func errorShaped(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	switch payload["error"].(type) {
	case map[string]any, string:
		return true
	}
	return first(payload["message"]) != "" && (first(payload["code"], payload["error_code"]) != "" || PayloadStatus(payload) != 0)
}

// First value given as text, skipping empty ones and objects
func first(values ...any) string {
	for _, v := range values {
		switch c := v.(type) {
		case nil, map[string]any, []any:
		case string:
			if c != "" {
				return c
			}
		case bool:
			if c {
				return "true"
			}
		case float64:
			return strconv.FormatFloat(c, 'f', -1, 64)
		default:
			return fmt.Sprint(c)
		}
	}
	return ""
}

func scalar(v any) string {
	return first(v)
}

// JSON object of text that holds one
func object(text string) map[string]any {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\xef\xbb\xbf"))
	if !strings.HasPrefix(text, "{") {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(text), &out) != nil {
		return nil
	}
	return out
}

// What a failed request is classified with: the provider and model it went to, whether the kind's own address served it, and the hints of
// the kind for a refusal there
type Refused struct {
	Route provider.Route
	Model string
	// Kind of the provider, for its hints and its address
	Spec provider.Spec
	// Handed each failure as it is classified, before the wire acts on it, such as the session's plugin hooks that classify failures; nil
	// for none
	Classify func(failure *provider.APIError)
}

// Error of a request the provider refused with the status and body
func (r Refused) HTTP(status int, body []byte, header http.Header) *provider.APIError {
	text := strings.ToValidUTF8(string(body), "�")
	raw := fmt.Sprintf("HTTP %d: %s", status, strs.Head(strings.TrimSpace(text), 500))
	if payload := object(text); payload != nil {
		if message := scalar(errorObject(payload)["message"]); message != "" {
			raw = fmt.Sprintf("HTTP %d: %s", status, strs.Head(message, 500))
		}
	} else if strings.TrimSpace(text) == "" {
		raw = fmt.Sprintf("HTTP %d: %s", status, http.StatusText(status))
	}
	failure := r.classify(status, text, header, errors.New(raw), raw)
	if failure.RetryAfter == 0 {
		failure.RetryAfter = RetryAfter(header)
	}
	return failure
}

// Error of a provider error inside a stream
func (r Refused) Stream(err *StreamError) *provider.APIError {
	encoded, _ := json.Marshal(err.Body)
	failure := r.classify(err.Status, string(encoded), err.Header, err, err.Error())
	if failure.Message == "" {
		failure.Message = err.Error()
	}
	if failure.RetryAfter == 0 {
		failure.RetryAfter = RetryAfter(err.Header)
	}
	return failure
}

// Error of a request that failed on its way, before or while the answer came; the context's own error when it was cancelled
func (r Refused) Transport(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.classify(0, "", nil, err, err.Error())
}

// Error of a reply that came back empty, with the message given
func (r Refused) Empty(message string, err error) *provider.APIError {
	failure := &provider.APIError{Provider: r.Route.Provider, Model: r.Model, Kind: provider.ErrEmptyResponse, Message: message, Err: err, Own: true}
	if r.Classify != nil {
		r.Classify(failure)
	}
	return failure
}

func (r Refused) classify(status int, body string, header http.Header, err error, raw string) *provider.APIError {
	payload := object(body)
	if status == 0 {
		status = bodyStatus(payload)
	}
	c := &classing{status: status, body: payload, header: header, err: err, provider: strings.ToLower(cmp.Or(r.Route.Spec, r.Route.Provider)), model: r.Model,
		message: messageOf(strings.ToLower(raw), payload, body), code: strings.ToLower(errorCode(payload))}
	kind := c.kind()
	message := extracted(raw, payload)
	own := watched(err)
	if timeout, ok := Summary(err, r.Route.Provider); ok && status == 0 && !watched(err) {
		message, own = timeout, true
	}
	if kind == provider.ErrAuth && !r.owns() {
		message += ". The request went to " + provider.BaseHost(r.Route.BaseURL)
	}
	if r.Spec.RejectionHint != nil {
		if hint := r.Spec.RejectionHint(status, r.Route.BaseURL, message); len(hint) > 0 {
			message += "\n" + strings.Join(hint, "\n")
		}
	}
	failure := &provider.APIError{Provider: r.Route.Provider, Model: r.Model, StatusCode: status, Message: message, Kind: kind, Err: err,
		Body: payload, Type: fmt.Sprintf("%T", err), Code: errorCode(payload), Own: own}
	if after, ok := retryAfterValue(errorObject(payload)["retry_after"]); ok && after > 0 {
		failure.RetryAfter = after
	} else if after, ok := retryAfterValue(payload["retry_after"]); ok && after > 0 {
		failure.RetryAfter = after
	} else if kind == provider.ErrRateLimit {
		failure.RetryAfter = c.reset()
	}
	if r.Classify != nil {
		r.Classify(failure)
	}
	return failure
}

// Whether the route's address is the kind's own, so a sign in refused there isn't one sent to another server
func (r Refused) owns() bool {
	host := provider.BaseHost(r.Route.BaseURL)
	if host == "" || r.Spec.Name == "" || host == provider.BaseHost(r.Spec.BaseURL) {
		return true
	}
	for _, domain := range r.Spec.Hostnames {
		if provider.BaseHostMatches(r.Route.BaseURL, domain) {
			return true
		}
	}
	return r.Spec.BaseURL == ""
}

// Wait a refusal's headers ask for: Retry-After in seconds or as a date, else x-ratelimit-reset
func RetryAfter(header http.Header) time.Duration {
	for _, name := range []string{"Retry-After", "x-ratelimit-reset"} {
		if wait, ok := httpx.RetryAfter(header.Get(name)); ok && wait > 0 {
			return wait
		}
	}
	return 0
}

// Wait a retry_after field of a body asks for, in seconds as a number or as text, or as a date
func retryAfterValue(v any) (time.Duration, bool) {
	switch n := v.(type) {
	case float64:
		return time.Duration(n * float64(time.Second)), n >= 0
	case string:
		return httpx.RetryAfter(n)
	}
	return 0, false
}

func errorObject(body map[string]any) map[string]any {
	if inner, ok := body["error"].(map[string]any); ok {
		return inner
	}
	return map[string]any{}
}

// Status a body names in its error object or code, 0 for none
func bodyStatus(body map[string]any) int {
	inner := errorObject(body)
	for _, candidate := range []any{inner["status_code"], inner["status"], inner["http_status"], inner["code"], body["code"]} {
		if f, ok := candidate.(float64); ok && f == float64(int(f)) && f >= 400 && f < 600 {
			return int(f)
		}
	}
	return 0
}

// Texts a body may give its message under, in order
func messages(body map[string]any) []string {
	if body == nil {
		return nil
	}
	out := []string{scalar(errorObject(body)["message"]), scalar(body["message"]), scalar(body["errorMessage"])}
	if args, ok := body["errorArgs"].(map[string]any); ok {
		out = append(out, scalar(args["reason"]))
	}
	switch detail := body["detail"].(type) {
	case map[string]any:
		out = append(out, scalar(detail["message"]))
	case string:
		out = append(out, detail)
	}
	return out
}

// Message of the error as the person reads it: the body's own, else the raw text
func extracted(raw string, body map[string]any) string {
	for _, m := range messages(body) {
		if strings.TrimSpace(m) != "" {
			return strs.Head(strings.TrimSpace(m), 500)
		}
	}
	return strs.Head(raw, 500)
}

// Lower case text a refusal is classified by: the raw error, the body's message, and the message of the upstream error OpenRouter wraps
func messageOf(raw string, body map[string]any, text string) string {
	parts := []string{raw}
	if body == nil {
		if t := strings.ToLower(strings.TrimSpace(text)); t != "" && !strings.Contains(raw, t) {
			parts = append(parts, strs.Head(t, 8000))
		}
		return strings.Join(parts, " ")
	}
	inner := errorObject(body)
	message := strings.ToLower(scalar(inner["message"]))
	message = cmp.Or(message, strings.ToLower(scalar(body["message"])))
	if message != "" && !strings.Contains(raw, message) {
		parts = append(parts, message)
	}
	if metadata, ok := inner["metadata"].(map[string]any); ok {
		wrapped, _ := metadata["raw"].(string)
		if upstream := strings.ToLower(scalar(errorObject(object(wrapped))["message"])); upstream != "" && !strings.Contains(raw, upstream) &&
			!strings.Contains(message, upstream) {
			parts = append(parts, upstream)
		}
	}
	return strings.Join(parts, " ")
}

// Code a body gives its error, such as rate_limit_exceeded
func errorCode(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	return codeOf(body, []string{"code", "error_code", "errorCode"}, true)
}

func codeOf(payload map[string]any, keys []string, nested bool) string {
	if payload == nil {
		return ""
	}
	if inner, ok := payload["error"].(map[string]any); ok {
		code := inner["code"]
		if code == nil || code == "" {
			code = inner["type"]
		}
		if _, text := code.(string); !text && inner["status"] != nil {
			code = inner["status"]
		}
		if s, ok := code.(string); ok && strings.TrimSpace(s) != "" && strings.TrimSpace(s) != "400" {
			return strings.TrimSpace(s)
		}
		if message, ok := inner["message"].(string); nested && ok && strings.HasPrefix(strings.TrimSpace(message), "{") {
			if code := codeOf(object(message), []string{"code", "error_code"}, false); code != "" {
				return code
			}
		}
	}
	for _, key := range keys {
		value := payload[key]
		if value == nil || value == "" || value == false {
			continue
		}
		text := ""
		switch t := value.(type) {
		case string:
			text = strings.TrimSpace(t)
		case float64:
			text = strconv.FormatFloat(t, 'f', -1, 64)
		}
		if text != "" && text != "400" {
			return text
		}
		break
	}
	return ""
}

// Whether a watch cut the request, whose error says why in its own words
func watched(err error) bool {
	_, stalled := errors.AsType[*StallError](err)
	_, quiet := errors.AsType[*QuietError](err)
	_, capped := errors.AsType[*LifetimeError](err)
	return stalled || quiet || capped
}

// Line naming the phase a request timed out in and the setting to raise, false for another error
func Summary(err error, provider string) (string, bool) {
	switch Phase(err) {
	case PhaseConnect:
		return "The connection to the provider took too long to open. Check the provider's Base URL in /providers and your network", true
	case PhaseRead:
		name := cmp.Or(strings.TrimSpace(provider), "the provider")
		return "The provider took too long to answer. If the model needs longer, raise Request time limit for " + name + " in /providers", true
	}
	return "", false
}

// Phase of a timeout in the error's chain, empty for another error
func Phase(err error) string {
	phase := ""
	for current := err; current != nil; current = errors.Unwrap(current) {
		switch e := current.(type) {
		case *provider.APIError:
			continue
		case *TimeoutError:
			if e.Phase == PhaseConnect {
				return PhaseConnect
			}
			phase = PhaseRead
			continue
		case *net.OpError:
			if !e.Timeout() {
				return phase
			}
			if e.Op == "dial" {
				return PhaseConnect
			}
			phase = PhaseRead
			continue
		}
		if current == context.DeadlineExceeded || current == os.ErrDeadlineExceeded {
			return PhaseRead
		}
		if timeout, ok := current.(interface{ Timeout() bool }); ok && timeout.Timeout() {
			phase = PhaseRead
			continue
		}
		if phase != "" {
			break
		}
	}
	return phase
}

// Failure on its way marked with the phase it timed out in, the error as it is when it is no timeout
func Timed(err error, phase string) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*TimeoutError](err); ok {
		return err
	}
	timeout := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
	if t, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); ok && t.Timeout() {
		timeout = true
	}
	if !timeout {
		return err
	}
	if found := Phase(err); found != "" {
		phase = found
	}
	return &TimeoutError{Phase: phase, Err: err}
}

// One request's refusal as the classifier reads it
type classing struct {
	status   int
	body     map[string]any
	header   http.Header
	err      error
	provider string
	model    string
	message  string
	code     string
}

func (c *classing) has(patterns []string) bool {
	return provider.Contains(c.message, patterns...)
}

// Kind of the refusal: the provider's special cases, then its status, its error code, its message, and how its connection failed
func (c *classing) kind() provider.ErrorKind {
	if c.err != nil && errors.Is(c.err, context.Canceled) {
		return provider.ErrUnknown
	}
	for _, stage := range []func() (provider.ErrorKind, bool){c.special, c.byStatus, c.byCode, c.byMessage, c.byTransport} {
		if kind, ok := stage(); ok {
			return kind
		}
	}
	return provider.ErrUnknown
}

func (c *classing) special() (provider.ErrorKind, bool) {
	msg, status := c.message, c.status
	switch {
	case c.code == "data_policy_denied":
		return provider.ErrInvalidRequest, true
	case c.has(contentPolicyBlocked):
		return provider.ErrContentFilter, true
	case c.has(accountPolicyBlock):
		return provider.ErrInvalidRequest, true
	case c.codexMaskedReplay():
		return provider.ErrInvalidRequest, true
	case status == 400 && (c.code == "thinking_signature_invalid" || strings.Contains(msg, "thinking_signature_invalid")):
		return provider.ErrInvalidRequest, true
	case status == 400 && strings.Contains(msg, "thinking") && (provider.Contains(msg, "signature", "cannot be modified", "must remain as they were") ||
		strings.Contains(msg, "redacted_thinking") && strings.Contains(msg, "invalid")):
		return provider.ErrInvalidRequest, true
	case status == 429 && strings.Contains(msg, "extra usage") && strings.Contains(msg, "long context"):
		return provider.ErrContextOverflow, true
	case status == 400 && strings.Contains(msg, "long context beta") && strings.Contains(msg, "not yet available"):
		return provider.ErrInvalidRequest, true
	case status == 400 && c.grammar() && !strings.Contains(msg, noUserQuery):
		return provider.ErrInvalidRequest, true
	case strings.Contains(msg, "do not have an active grok subscription") || strings.Contains(msg, "out of available resources") && strings.Contains(msg, "grok"):
		return provider.ErrAuth, true
	}
	return "", false
}

// A llama.cpp server that couldn't turn the request's schema into a grammar
func (c *classing) grammar() bool {
	msg := c.message
	return strings.Contains(msg, "error parsing grammar") || strings.Contains(msg, "json-schema-to-grammar") ||
		strings.Contains(msg, "unable to generate parser") && strings.Contains(msg, "template") ||
		strings.Contains(msg, "invalid json schema") && strings.Contains(msg, "regex lookaround") && strings.Contains(msg, "not supported")
}

// The ChatGPT Codex backend refusing a replay whose content it masked
func (c *classing) codexMaskedReplay() bool {
	if c.provider != "openai-codex" || c.status != 0 && c.status != 400 {
		return false
	}
	if detail, _ := c.body["detail"].(string); strings.ToLower(strings.TrimSpace(detail)) == "unsupported content type" ||
		len(c.body) == 0 && strings.Contains(c.message, "unsupported content type") && strings.Contains(c.message, "detail") {
		return true
	}
	bodyMessage := ""
	for _, candidate := range messages(c.body) {
		if candidate != "" {
			bodyMessage = strings.ToLower(strings.TrimSpace(candidate))
			break
		}
	}
	return c.code == "invalid_prompt" && bodyMessage == "request blocked." || strings.TrimSpace(c.message) == "invalid_prompt: request blocked."
}

func (c *classing) byStatus() (provider.ErrorKind, bool) {
	switch status := c.status; {
	case status == 0:
		return "", false
	case status == 400:
		return c.bad(), true
	case status == 401:
		return provider.ErrAuth, true
	case status == 402:
		return c.payment(), true
	case status == 403:
		switch {
		case c.code == "upstream_unavailable":
			return provider.ErrServer, true
		case c.provider == "xai-oauth" && c.code == xaiSpendingLimit || c.has(append([]string{"key limit exceeded", "spending limit"}, billing...)):
			return provider.ErrQuota, true
		}
		return provider.ErrAuth, true
	case status == 404:
		switch {
		case billingCodes[c.code] || c.has(billing):
			return provider.ErrQuota, true
		case c.has(policyBlocked):
			return provider.ErrInvalidRequest, true
		case c.has(modelNotFound):
			return provider.ErrModelNotFound, true
		}
		return provider.ErrUnknown, true
	case status == 408:
		return provider.ErrNetwork, true
	case status == 413:
		return provider.ErrContextOverflow, true
	case status == 422:
		return provider.ErrInvalidRequest, true
	case status == 429:
		return c.limited(), true
	case status == 500 || status == 502:
		if (c.has(requestValidation) || c.code == "unknown_parameter" || c.code == "unsupported_parameter" || c.code == "invalid_request_error") &&
			!c.injected() {
			return provider.ErrInvalidRequest, true
		}
		if kind, ok := c.overflowing(); ok {
			return kind, true
		}
		return provider.ErrServer, true
	case status == 503 || status == 529:
		if kind, ok := c.overflowing(); ok {
			return kind, true
		}
		return provider.ErrServer, true
	case status >= 400 && status < 500:
		return provider.ErrInvalidRequest, true
	case status >= 500 && status < 600:
		return provider.ErrServer, true
	}
	return "", false
}

// A server error that is an empty reply, a full memory or a context overflow underneath
func (c *classing) overflowing() (provider.ErrorKind, bool) {
	switch {
	case c.has(emptyResponse), c.has(memoryCeiling):
		return provider.ErrServer, true
	case c.has(contextOverflow):
		return provider.ErrContextOverflow, true
	}
	return "", false
}

func (c *classing) bad() provider.ErrorKind {
	msg := c.message
	switch {
	case c.has(multimodalTool), c.has(imageCorrupt), c.has(imageTooLarge):
		return provider.ErrInvalidRequest
	case strings.Contains(msg, "model is not supported when using codex with a chatgpt account"):
		return provider.ErrModelNotFound
	case c.encrypted():
		return provider.ErrInvalidRequest
	case strings.Contains(msg, "reasoning is mandatory"):
		return provider.ErrInvalidRequest
	case c.injected():
		return provider.ErrServer
	case c.has(malformedArgs), c.has(roleAlternation), c.has(validation400), c.code == "unknown_parameter", c.code == "unsupported_parameter",
		c.has(invalidBody), c.code == "invalid_request_body":
		return provider.ErrInvalidRequest
	case memoryCodes[c.code]:
		return provider.ErrServer
	}
	if kind, ok := c.overflowing(); ok {
		return kind
	}
	switch {
	case c.has(policyBlocked):
		return provider.ErrInvalidRequest
	case c.has(modelNotFound):
		return provider.ErrModelNotFound
	case c.has(rateLimit):
		return provider.ErrRateLimit
	case c.has(billing):
		return provider.ErrQuota
	}
	return provider.ErrInvalidRequest
}

// A refusal of a replayed encrypted reasoning item
func (c *classing) encrypted() bool {
	msg := c.message
	return c.code == "invalid_encrypted_content" || strings.Contains(msg, "invalid_encrypted_content") ||
		strings.Contains(msg, "encrypted content for item") && strings.Contains(msg, "could not be verified") ||
		strings.Contains(msg, "could not decrypt the provided encrypted_content") ||
		strings.Contains(msg, "encrypted content could not be decrypted or parsed") ||
		strings.Contains(msg, "encrypted_content") && strings.Contains(msg, "was not issued to this caller") ||
		strings.Contains(msg, "conflicting authenticated continuation identities")
}

// A parameter the provider's own server put in the request and then refused, which no change of the request fixes
func (c *classing) injected() bool {
	if !strings.Contains(c.message, "prompt_cache_retention") || !c.has([]string{"not supported", "unsupported", "unknown", "unrecognized"}) {
		return false
	}
	return !provider.Contains(c.provider, "meta", "muse", "msl", "model-api", "bedrock", "mantle")
}

// A 402: a usage limit that resets is a rate limit, anything else wants credit
func (c *classing) payment() provider.ErrorKind {
	if c.has(usageLimit) && c.has(usageLimitTransient) {
		return provider.ErrRateLimit
	}
	return provider.ErrQuota
}

func (c *classing) limited() provider.ErrorKind {
	switch {
	case billingCodes[c.code]:
		return provider.ErrQuota
	case c.has(overloaded):
		return provider.ErrServer
	}
	wall := c.code == "usage_limit_reached" || c.has(append(append([]string{"usage_limit_reached"}, usageLimit...), billing...))
	if wall && !c.has(rateLimit) && !c.transient() {
		return provider.ErrQuota
	}
	return provider.ErrRateLimit
}

// Whether a usage limit says when it lifts, so it is waited out rather than paid
func (c *classing) transient() bool {
	if c.has(usageLimitTransient) {
		return true
	}
	for _, payload := range []map[string]any{c.body, errorObject(c.body)} {
		for _, field := range resetFields {
			if v, ok := payload[field]; ok && v != nil && v != "" {
				return true
			}
		}
	}
	return c.header.Get("Retry-After") != "" || c.header.Get("x-ratelimit-reset") != ""
}

// Wait until a rate limit lifts: the body's reset fields, the headers, then the message
func (c *classing) reset() time.Duration {
	for _, payload := range []map[string]any{c.body, errorObject(c.body)} {
		for _, field := range resetFields {
			value, ok := payload[field]
			if !ok || value == nil || value == "" {
				continue
			}
			if at, number := value.(float64); number && strings.HasSuffix(field, "_at") {
				return max(time.Until(time.Unix(0, int64(at*float64(time.Second)))), 0)
			}
			if after, ok := retryAfterValue(value); ok {
				return after
			}
		}
	}
	if after := RetryAfter(c.header); after > 0 {
		return after
	}
	after, _ := credentials.Delay(c.message)
	return after
}

func (c *classing) byCode() (provider.ErrorKind, bool) {
	switch code := c.code; {
	case code == CodeNotJSON && strings.Contains(c.message, "request validation failed:"):
		return provider.ErrInvalidRequest, true
	case code == "resource_exhausted", code == "throttled", code == "rate_limit_exceeded", code == "rate_limit_error":
		return provider.ErrRateLimit, true
	case code == "model_not_found", code == "model_not_available", code == "invalid_model":
		return provider.ErrModelNotFound, true
	case code == "context_length_exceeded", code == "max_tokens_exceeded":
		return provider.ErrContextOverflow, true
	case code == "invalid_encrypted_content":
		return provider.ErrInvalidRequest, true
	case billingCodes[code]:
		return provider.ErrQuota, true
	case memoryCodes[code], code == "server_error", code == "unavailable", code == "internal", code == "api_error":
		return provider.ErrServer, true
	case code == "deadline_exceeded":
		return provider.ErrNetwork, true
	}
	return "", false
}

func (c *classing) byMessage() (provider.ErrorKind, bool) {
	switch {
	case c.has(memoryCeiling):
		return provider.ErrServer, true
	case c.has(payloadTooLarge):
		return provider.ErrContextOverflow, true
	case c.has(roleAlternation), c.has(renderError), c.has(multimodalTool), c.has(imageCorrupt), c.has(imageTooLarge):
		return provider.ErrInvalidRequest, true
	case c.has(usageLimit):
		return c.payment(), true
	case c.has(overloaded):
		return provider.ErrServer, true
	case c.has(billing):
		return provider.ErrQuota, true
	case c.has(rateLimit):
		return provider.ErrRateLimit, true
	case c.has(emptyResponse):
		return provider.ErrServer, true
	case c.has(contextOverflow):
		return provider.ErrContextOverflow, true
	case c.has(auth):
		return provider.ErrAuth, true
	case c.has(policyBlocked):
		return provider.ErrInvalidRequest, true
	case c.has(modelNotFound):
		return provider.ErrModelNotFound, true
	case c.has(timeoutMessages), c.has(connectionMessages):
		return provider.ErrNetwork, true
	}
	return "", false
}

func (c *classing) byTransport() (provider.ErrorKind, bool) {
	switch {
	case c.has(certificateFailures), c.has(tlsTransient), certificate(c.err):
		return provider.ErrNetwork, true
	case c.status == 0 && c.has(disconnects):
		return provider.ErrNetwork, true
	case strings.Contains(c.message, "in a row, so icebath stopped asking"):
		return provider.ErrNetwork, true
	case unreached(c.err):
		return provider.ErrNetwork, true
	}
	return "", false
}

// A failure of the connection itself rather than an answer of the provider
func unreached(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*TimeoutError](err); ok {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

func certificate(err error) bool {
	if err == nil {
		return false
	}
	_, unknown := errors.AsType[x509.UnknownAuthorityError](err)
	_, invalid := errors.AsType[x509.CertificateInvalidError](err)
	_, host := errors.AsType[x509.HostnameError](err)
	return unknown || invalid || host
}
