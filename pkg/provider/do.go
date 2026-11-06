package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Env var bounding the stream level reconnects before an error reaches the main loop
const StreamRetriesEnv = "ICEBATH_STREAM_RETRIES"

// Request metadata key an adapter reads to omit stream_options for the session
const StreamOptionsOffKey = "icebath.stream_options"

// One non streaming unmask probe per window after a streaming 5xx
const stream5xxProbeWindow = 60 * time.Second

// Default retry budget of one API call block
const DefaultMaxRetries = 3

// Default post exhaustion auto recovery cycles
const DefaultAutoRecoveryCycles = 5

// Activity heartbeat spacing during a long retry wait
const activityHeartbeat = 30 * time.Second

var (
	overflowReasons         = map[FailoverReason]bool{ReasonLongContextTier: true, ReasonPayloadTooLarge: true, ReasonContextOverflow: true}
	transportFailureReasons = map[FailoverReason]bool{ReasonTimeout: true, ReasonOverloaded: true}
	ladderReasons           = map[FailoverReason]bool{ReasonOverloaded: true, ReasonServerError: true, ReasonTimeout: true}
	streamDropMarkers       = []string{"connection lost", "connection reset", "connection closed", "network connection", "network error", "terminated"}
	thinkingTimeoutMarkers  = []string{"broken pipe", "errno 32", "remote protocol", "connection reset", "connection lost", "peer closed", "server disconnected"}
	sseConnPhrases          = []string{"connection lost", "connection reset", "connection closed", "connection terminated", "network error", "network connection", "terminated", "peer closed", "broken pipe", "upstream connect error"}
	zaiPolicyNotes          = map[string]string{"zai_coding_overload_long": " (Z.AI Coding overload adaptive long backoff)", "zai_coding_overload_short": " (Z.AI Coding overload short retry)"}
	nonretryableLabels      = map[FailoverReason]string{
		ReasonContentPolicyBlocked: "The provider's safety filter refused this request",
		ReasonUpstreamBlocked:      "A firewall/CDN in front of the provider blocked this request",
		ReasonSSLCertVerification:  "The provider's security certificate could not be verified",
		ReasonImageTooLarge:        "Request still exceeded the provider's size limit after shrinking images",
	}
	ladderStopHints     = map[string]string{"cli": "press Esc to stop", "tui": "press Esc to stop", "desktop": "press Esc to stop", "api_server": "cancel the request to stop", "cron": ""}
	vendorResetHeaders  = []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset"}
	durationComponentRe = regexp.MustCompile(`(\d+(?:\.\d+)?)(ms|h|m|s)`)
	durationUnitSeconds = map[string]float64{"h": 3600, "m": 60, "s": 1, "ms": 0.001}
)

const ladderDefaultStopHint = "send /stop to cancel"

// Free tier rate limit copy for the chat and the card
const (
	FreeTierRateLimitCard  = "You've used up the allowance for chatting without signing in. It refreshes in {reset}. Sign in for a bigger allowance, it's free."
	FreeTierRateLimitChat  = FreeTierRateLimitCard + " To sign in: /login."
	ContentPolicyNextSteps = "Try rewording your message or removing sensitive attachments, or switch to another model with /model."
)

// errorTypeName names the wrapped transport error type or the APIError itself
func errorTypeName(apiErr *APIError) string {
	if apiErr == nil {
		return "nil"
	}
	if apiErr.Err != nil {
		return fmt.Sprintf("%T", apiErr.Err)
	}
	return "APIError"
}

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

// ExtractAPIErrorContext pulls reason, message, and reset_at out of a provider error body, headers, and text
func ExtractAPIErrorContext(err error) map[string]any {
	ctx := map[string]any{}
	if err == nil {
		return ctx
	}
	now := time.Now()
	var apiErr *APIError
	var body map[string]any
	if errors.As(err, &apiErr) {
		body = decodeJSONObject(apiErr.Body)
	}
	payload := body
	if nested, ok := body["error"].(map[string]any); ok {
		payload = nested
	}
	if payload != nil {
		for _, key := range []string{"code", "type", "error"} {
			if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
				ctx["reason"] = strings.TrimSpace(s)
				break
			}
		}
		message := stringField(payload, "message")
		if message == "" {
			message = stringField(payload, "error_description")
		}
		if message == "" {
			if s, ok := payload["error"].(string); ok {
				message = s
			}
		}
		if strings.TrimSpace(message) != "" {
			ctx["message"] = strings.TrimSpace(message)
		}
		for _, key := range []string{"resets_at", "reset_at"} {
			if v, ok := payload[key]; ok && v != nil && v != "" {
				ctx["reset_at"] = v
				break
			}
		}
		if _, has := ctx["reset_at"]; !has {
			if secs, ok := payload["resets_in_seconds"].(float64); ok {
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

// RetryState carries the one shot recovery guards of a single API call block
type RetryState struct {
	CodexAuthRetryAttempted               bool
	AnthropicAuthRetryAttempted           bool
	NousAuthRetryAttempted                bool
	NousPaidEntitlementRefreshAttempted   bool
	WelcomeModelSwitchAttempted           bool
	WelcomeRouteHealAttempted             bool
	ProviderAuthRefreshAttempted          bool
	ProviderStaleRefreshAttempted         bool
	VertexAuthRetryAttempted              bool
	ThinkingSigRetryAttempted             bool
	InvalidEncryptedContentRetryAttempted bool
	NativeCompactionRejectRetryAttempted  bool
	ImageShrinkRetryAttempted             bool
	MultimodalToolContentRetryAttempted   bool
	ReasoningMandatoryRetryAttempted      bool
	OAuth1MBetaRetryAttempted             bool
	LlamaCppGrammarRetryAttempted         bool
	PrimaryRecoveryAttempted              bool
	HasRetried429                         bool
	Affordable402ClampAttempted           bool
	AuthFailoverAttempted                 bool
	AutoRecoveryCyclesUsed                int
	UnicodeSanitizationPasses             int
	RestartWithCompressedMessages         bool
	RestartWithLengthContinuation         bool
	RestartWithRebuiltMessages            bool
	RestartWithRedirectedMessages         bool
	// Live request and route the recovery hooks may rewrite
	Route   Route
	Request *Request
	Client  Client
	// Retry loop counters the hooks may read
	RetryCount          int
	MaxRetries          int
	CompressionAttempts int
}

// RetrySession holds the latches that outlive one API call block
type RetrySession struct {
	DisableStreaming         bool
	StreamOptionsUnsupported bool
	stream5xxProbeAt         time.Time
	stream5xxProbed          bool
	retryStatusBuffer        []string
	staleStreak              atomic.Int64
}

// OverflowVerdict is the agent's answer to a context overflow or long context tier rejection
type OverflowVerdict struct {
	// Retry now with the rebuilt request
	Handled bool
	// Restart the block after a compression with the request rebuilt
	Restart bool
	// The error was a context length problem even when not handled
	IsContextLengthError bool
	// Terminal failure the agent produced
	Err error
}

// DoOptions wires the retry loop to the chain, pool, hooks, surfaces, and the agent's message level recoveries
type DoOptions struct {
	Chain   *Chain
	Rotator Rotator
	Hooks   Hooks
	Credits *CreditsCapture
	Fast    *FastWindow
	Session *RetrySession
	Stream  StreamFunc
	// Builds the client for a route after a fallback or rotation, defaults to NewClient
	ClientFor func(ctx context.Context, route Route) (Client, error)
	// Rebuilds the request for a new route before the block restarts, defaults to a model rename
	Rebuild func(ctx context.Context, route Route, req *Request) (*Request, error)
	// Agent level recoveries in hermes order, each returns true to retry now
	RecoverBeforeClassification func(ctx context.Context, st *RetryState, err error) bool
	RecoverBeforeRotation       func(ctx context.Context, st *RetryState, cl *Classified, apiErr *APIError) bool
	RecoverAfterRotation        func(ctx context.Context, st *RetryState, cl *Classified, apiErr *APIError) bool
	// Stale credential self heal of a provider profile, true when a fresh credential was installed
	RefreshStaleCredential func(ctx context.Context, st *RetryState, apiErr *APIError) (notice string, refreshed bool)
	// Stale credential test, nil asks the profile owning the route credential refresh
	IsStaleCredentialError func(route Route, status int, message string) bool
	OnLongContextTier      func(ctx context.Context, st *RetryState, cl *Classified, apiErr *APIError) OverflowVerdict
	OnOverflow             func(ctx context.Context, st *RetryState, cl *Classified, apiErr *APIError, wrappedOutputCap int) OverflowVerdict
	// Called after a rotation or fallback changed the route and before the block restarts
	OnRouteChange func(ctx context.Context, route Route)
	// Applied once a nous model switch header moves the session model
	OnModelSwitch func(ctx context.Context, requested, backing string)
	// Called after every retry wait so a short lived credential can be re adopted
	AfterWait func(ctx context.Context)
	// Streamed assistant text so far, read by the recovery ladder
	StreamedText func() string
	// Surfaces
	Status       func(string)
	BufferStatus func(string)
	FlushStatus  func()
	Wait         func(string)
	Activity     func(string)
	Verbose      func(lines ...string)
	Warning      func(string)
	DebugDump    func(req *Request, reason string, err error)
	// Identity
	Home               string
	ProfileName        string
	Platform           string
	SessionID          string
	TaskID             string
	TurnID             string
	APIRequestID       string
	APICallCount       int
	ContextLength      int
	ApproxTokens       int
	CompressionEnabled bool
	Attended           bool
	MaxRetries         int
	AutoRecoveryCycles int
	VerboseLogging     bool
	Deadline           time.Duration
	Log                *slog.Logger
	Now                func() time.Time
	Sleep              func(ctx context.Context, d time.Duration) error
}

// DoFailure names why the retry loop gave up
type DoFailure string

const (
	FailExhausted          DoFailure = "max_retries_exhausted"
	FailNonRetryable       DoFailure = "non_retryable_client_error"
	FailInterrupted        DoFailure = "interrupted"
	FailNousRateLimited    DoFailure = "nous_rate_limited"
	FailCompactionDisabled DoFailure = "compaction_disabled"
	FailPartialStream      DoFailure = "partial_stream"
	FailDeadline           DoFailure = "deadline"
	FailOverflow           DoFailure = "overflow"
)

// DoError is the terminal outcome of a retry loop with everything the turn copy needs
type DoError struct {
	Failure           DoFailure
	Err               error
	APIError          *APIError
	Classified        *Classified
	Summary           string
	Attempts          int
	Reason            FailoverReason
	Retryable         bool
	BillingUnverified bool
	IsRateLimited     bool
	IsStreamDrop      bool
	IsThinkingTimeout bool
	PrefixSuggestion  string
	ResetHint         string
	ResetAt           float64
	HasResetAt        bool
	FreeTierKind      string
	WelcomeHint       string
	UsageWindow       bool
	Guidance          []string
	Route             Route
	Message           string
	DeltasSent        bool
}

func (e *DoError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Summary != "" {
		return e.Summary
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Failure)
}

func (e *DoError) Unwrap() error { return e.Err }

type doLoop struct {
	ctx      context.Context
	opts     DoOptions
	client   Client
	req      *Request
	route    Route
	st       *RetryState
	session  *RetrySession
	chain    *Chain
	cooldown *Cooldown
	log      *slog.Logger
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error

	retryCount          int
	maxRetries          int
	compressionAttempts int
	streamedText        string
}

func (l *doLoop) status(s string) {
	if l.opts.Status != nil {
		l.opts.Status(s)
	}
}

func (l *doLoop) bufferStatus(s string) {
	l.session.retryStatusBuffer = append(l.session.retryStatusBuffer, s)
	if l.opts.BufferStatus != nil {
		l.opts.BufferStatus(s)
	}
}

func (l *doLoop) flushStatus() {
	l.session.retryStatusBuffer = nil
	if l.opts.FlushStatus != nil {
		l.opts.FlushStatus()
	}
}

func (l *doLoop) verbose(lines ...string) {
	if l.opts.Verbose != nil {
		l.opts.Verbose(lines...)
	}
}

func (l *doLoop) activity(s string) {
	if l.opts.Activity != nil {
		l.opts.Activity(s)
	}
}

func (l *doLoop) wait(s string) {
	if l.opts.Wait != nil {
		l.opts.Wait(s)
	}
}

func (l *doLoop) logContext() string {
	return fmt.Sprintf("provider=%s model=%s base_url=%s", l.route.Provider, l.route.Model, l.route.BaseURL)
}

func (l *doLoop) anonymous() bool {
	return IsAnonymousRequest(l.route.Provider, l.route.APIKey)
}

func (l *doLoop) onFreeModel() bool {
	return RouteIsWelcomeHost(l.route.BaseURL) && l.anonymous()
}

func (l *doLoop) isOpenRouterURL() bool {
	return HostMatches(l.route.BaseURL, "openrouter.ai")
}

func (l *doLoop) identity() HookIdentity {
	return HookIdentity{TaskID: l.opts.TaskID, TurnID: l.opts.TurnID, APIRequestID: l.opts.APIRequestID, SessionID: l.opts.SessionID,
		Platform: l.opts.Platform, Model: l.route.Model, Provider: l.route.Provider, BaseURL: l.route.BaseURL, APIMode: l.route.Mode, APICallCount: l.opts.APICallCount}
}

// switchRoute rebuilds the client and the request for a new route
func (l *doLoop) switchRoute(route Route) error {
	client, err := l.clientFor(route)
	if err != nil {
		return err
	}
	req := l.req
	if l.opts.Rebuild != nil {
		req, err = l.opts.Rebuild(l.ctx, route, l.req)
		if err != nil {
			return err
		}
	} else {
		cp := *l.req
		cp.Model = route.Model
		req = &cp
	}
	l.route, l.client, l.req = route, client, req
	l.st.Route, l.st.Client, l.st.Request = route, client, req
	if l.opts.Credits != nil {
		l.opts.Credits.SetRoute(route.Provider, route.Model, route.BaseURL)
	}
	if l.opts.OnRouteChange != nil {
		l.opts.OnRouteChange(l.ctx, route)
	}
	return nil
}

func (l *doLoop) clientFor(route Route) (Client, error) {
	if l.opts.ClientFor != nil {
		return l.opts.ClientFor(l.ctx, route)
	}
	return NewClient(route)
}

// restartAfterFallback switches onto the chain's current route and zeroes the block counters
func (l *doLoop) restartAfterFallback() error {
	l.st.PrimaryRecoveryAttempted = false
	l.st.RestartWithRebuiltMessages = true
	l.retryCount = 0
	l.compressionAttempts = 0
	l.session.ResetStaleStreak()
	return l.switchRoute(l.chain.Current())
}

func (l *doLoop) tryFallback(reason FailoverReason, resetAt any) (bool, error) {
	if l.chain == nil {
		return false, nil
	}
	if !l.chain.TryActivateFallback(l.ctx, reason, resetAt) {
		return false, nil
	}
	if err := l.restartAfterFallback(); err != nil {
		return false, err
	}
	return true, nil
}

func (l *doLoop) hasPendingFallback() bool {
	return l.chain != nil && l.chain.HasPending()
}

func resetAtOf(ctx map[string]any) any {
	if v, ok := ctx["reset_at"]; ok {
		return v
	}
	return nil
}

func usageWindowOf(cl *Classified) bool {
	if cl == nil || cl.Context == nil {
		return false
	}
	v, _ := cl.Context["usage_window"].(bool)
	return v
}

// asAPIError classifies a raw adapter error into the route's APIError when it is not one already
func (l *doLoop) asAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Provider == "" {
			apiErr.Provider = l.route.Provider
		}
		if apiErr.Model == "" {
			apiErr.Model = l.route.Model
		}
		return apiErr
	}
	return Classify(l.route.Provider, l.route.Model, 0, "", err)
}

func (l *doLoop) classify(apiErr *APIError) *Classified {
	msgs := 0
	if l.req != nil {
		msgs = len(l.req.Messages)
	}
	ctxLen := l.opts.ContextLength
	if ctxLen <= 0 {
		ctxLen = 200000
	}
	return ClassifyError(ClassifyInput{Err: apiErr, Status: apiErr.StatusCode, Body: apiErr.Body, Provider: l.route.Provider, Model: l.route.Model,
		ApproxTokens: l.opts.ApproxTokens, ContextLength: ctxLen, NumMessages: msgs, BaseURL: l.route.BaseURL, APIKey: l.route.APIKey, Headers: errorHeaders(apiErr)})
}

// nousRateLimitGuard skips the call while another session recorded a Nous Portal rate limit
func (l *doLoop) nousRateLimitGuard() (*DoError, bool, error) {
	if l.route.Provider != "nous" {
		return nil, false, nil
	}
	if l.opts.Credits != nil {
		if requested, backing, ok := l.opts.Credits.TakePendingModelSwitch(); ok && l.route.Model == requested {
			l.route.Model = backing
			l.req.Model = backing
			l.st.Route = l.route
			l.log.Info(fmt.Sprintf("Nous gateway asked to switch %s -> %s; applied for this session", requested, backing))
			if l.opts.OnModelSwitch != nil {
				l.opts.OnModelSwitch(l.ctx, requested, backing)
			}
			l.bufferStatus(fmt.Sprintf("Model is now %s (your account's model; %s is the free tier's).", backing, requested))
		}
	}
	anonymous := l.anonymous()
	guard := &NousRateGuard{Home: l.opts.Home, Anonymous: anonymous, Now: l.now}
	remaining, ok := guard.Remaining()
	if !ok || remaining <= 0 {
		return nil, false, nil
	}
	var msg string
	if anonymous {
		msg = strings.ReplaceAll(FreeTierRateLimitChat, "{reset}", FriendlyWait(int(math.Ceil(remaining.Seconds()))))
	} else {
		msg = fmt.Sprintf("Your Nous account has hit its rate limit; it resets in %s.", FormatSeconds(remaining.Seconds()))
	}
	l.verbose(fmt.Sprintf("⏳ %s Trying fallback...", msg))
	l.bufferStatus("⏳ " + msg)
	if switched, err := l.tryFallback("", nil); err != nil {
		return nil, false, err
	} else if switched {
		return nil, true, nil
	}
	l.flushStatus()
	de := &DoError{Failure: FailNousRateLimited, Reason: ReasonRateLimit, Retryable: true, Message: msg, Summary: msg, Route: l.route}
	if anonymous {
		de.FreeTierKind = "rate_limited"
		de.WelcomeHint = strings.ReplaceAll(FreeTierRateLimitCard, "{reset}", FriendlyWait(int(math.Ceil(remaining.Seconds()))))
	}
	return de, false, nil
}

func (l *doLoop) captureSuccessHeaders(resp *Response) {
	if l.opts.Credits != nil && resp != nil && len(resp.Headers) > 0 {
		l.opts.Credits.CaptureRateLimits(resp.Headers)
		l.opts.Credits.CaptureCredits(resp.Headers, false)
		l.opts.Credits.CheckOpenRouterCacheStatus(resp.Headers)
		l.opts.Credits.CaptureNousModelSwitch(resp.Headers)
	}
	if l.route.Provider == "nous" {
		guard := &NousRateGuard{Home: l.opts.Home, Anonymous: l.anonymous(), Now: l.now}
		if err := guard.Clear(); err != nil {
			l.log.Debug(fmt.Sprintf("nous rate guard clear failed: %v", err))
		}
	}
}

func (l *doLoop) firePreRequest(start time.Time) {
	if l.opts.Hooks == nil || !l.opts.Hooks.Has(HookPreAPIRequest) {
		return
	}
	chars := 0
	systemPrompt := ""
	for _, m := range l.req.Messages {
		if m == nil {
			continue
		}
		text := m.Text()
		chars += len(text)
		if m.Role == "system" && systemPrompt == "" {
			systemPrompt = text
		}
	}
	l.opts.Hooks.PreRequest(l.ctx, &PreRequestPayload{HookIdentity: l.identity(), ConversationHistory: l.req.Messages, RetryCount: l.retryCount,
		RequestMessages: l.req.Messages, SystemPrompt: systemPrompt, MessageCount: len(l.req.Messages), ToolCount: len(l.req.Tools),
		ApproxInputTokens: l.opts.ApproxTokens, RequestCharCount: chars, MaxTokens: l.req.MaxTokens, StartedAt: start, Request: RequestPayloadForHook(l.req)})
}

func (l *doLoop) firePostRequest(start, firstChunk time.Time, resp *Response) {
	if l.opts.Hooks == nil || !l.opts.Hooks.Has(HookPostAPIRequest) || resp == nil {
		return
	}
	end := l.now()
	p := &PostRequestPayload{HookIdentity: l.identity(), APIDuration: end.Sub(start), StartedAt: start, EndedAt: end, FirstChunkAt: firstChunk,
		FinishReason: resp.FinishReason, MessageCount: len(l.req.Messages), ResponseModel: resp.Model, Response: ResponsePayloadForHook(resp, resp.FinishReason),
		Usage: UsageSummaryForHook(resp), ContextLength: l.opts.ContextLength, AssistantMessage: resp.Message}
	if resp.Message != nil {
		p.AssistantContentChars = len(resp.Message.Text())
		p.AssistantToolCallCount = len(resp.Message.ToolCalls)
	}
	l.opts.Hooks.PostRequest(l.ctx, p)
}

func (l *doLoop) fireErrorHook(start time.Time, apiErr *APIError, cl *Classified) {
	InvokeAPIRequestErrorHook(l.ctx, l.opts.Hooks, &ErrorPayload{HookIdentity: l.identity(), StartedAt: start, StatusCode: apiErr.StatusCode,
		RetryCount: l.retryCount, MaxRetries: l.maxRetries, Retryable: cl.Retryable, Reason: cl.Reason,
		ErrorType: errorTypeName(apiErr), ErrorMessage: apiErr.Error()}, l.req)
}

// Do runs one inference call with the retry, rotation, fallback, and backoff policy every adapter shares
func Do(ctx context.Context, client Client, req *Request, opts DoOptions) (*Response, error) {
	if client == nil {
		return nil, errors.New("provider: Do needs a client")
	}
	if req == nil {
		return nil, errors.New("provider: Do needs a request")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = deadlineSleep
	}
	if opts.Session == nil {
		opts.Session = &RetrySession{}
	}
	ctx = withStaleStreak(ctx, opts.Session)
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = DefaultMaxRetries
	}
	if opts.AutoRecoveryCycles < 0 {
		opts.AutoRecoveryCycles = 0
	}
	route := client.Route()
	cooldown := NewCooldown(nil)
	if opts.Chain != nil {
		cooldown = opts.Chain.Cooldown()
		opts.Chain.SetCurrent(route)
	}
	if opts.Chain != nil && !opts.Chain.HasDiagnosticSink() && opts.BufferStatus != nil {
		opts.Chain.SetDiagnostic(opts.BufferStatus)
	}
	l := &doLoop{ctx: ctx, opts: opts, client: client, req: req, route: route, session: opts.Session, chain: opts.Chain, cooldown: cooldown,
		log: opts.Log, now: opts.Now, sleep: opts.Sleep, maxRetries: opts.MaxRetries}
	l.st = &RetryState{Route: route, Request: req, Client: client, MaxRetries: opts.MaxRetries}
	if opts.Credits != nil {
		opts.Credits.SetRoute(route.Provider, route.Model, route.BaseURL)
	}
	if opts.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, opts.Deadline, &DeadlineExpiredError{Label: "api_call", After: opts.Deadline})
		defer cancel()
		l.ctx = ctx
	}
	return l.run()
}

func (l *doLoop) run() (*Response, error) {
	for {
		if de, restarted, err := l.nousRateLimitGuard(); err != nil {
			return nil, err
		} else if de != nil {
			return nil, de
		} else if restarted {
			continue
		}
		l.st.RetryCount, l.st.MaxRetries, l.st.CompressionAttempts = l.retryCount, l.maxRetries, l.compressionAttempts
		start := l.now()
		l.firePreRequest(start)
		resp, firstChunk, err := l.call()
		if err == nil {
			l.firePostRequest(start, firstChunk, resp)
			l.captureSuccessHeaders(resp)
			l.session.ResetStaleStreak()
			l.activity(fmt.Sprintf("API call #%d completed", l.opts.APICallCount))
			return resp, nil
		}
		if de := l.interrupted(err, start); de != nil {
			return nil, de
		}
		var partial *PartialStreamError
		if errors.As(err, &partial) {
			apiErr := l.asAPIError(partial.Err)
			return nil, &DoError{Failure: FailPartialStream, Err: err, APIError: apiErr, Summary: SummarizeAPIError(partial.Err, l.route.Provider), Route: l.route, DeltasSent: true, Message: SummarizeAPIError(partial.Err, l.route.Provider)}
		}
		de, continueLoop, loopErr := l.handleError(err, start)
		if loopErr != nil {
			return nil, loopErr
		}
		if de != nil {
			return nil, de
		}
		if !continueLoop {
			return nil, errors.New("provider: retry loop ended without an outcome")
		}
	}
}

// interrupted turns a cancelled context into the interrupted or deadline outcome
func (l *doLoop) interrupted(err error, start time.Time) *DoError {
	if l.ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	cause := context.Cause(l.ctx)
	var expired *DeadlineExpiredError
	if errors.As(cause, &expired) || errors.As(err, &expired) {
		if expired == nil {
			expired = &DeadlineExpiredError{Label: "api_call", After: l.opts.Deadline}
		}
		return &DoError{Failure: FailDeadline, Err: expired, Reason: ReasonTimeout, Retryable: true, Route: l.route, Summary: expired.Error(), Message: expired.Error()}
	}
	elapsed := l.now().Sub(start)
	msg := fmt.Sprintf("Operation interrupted: waiting for model response (%.1fs elapsed).", elapsed.Seconds())
	l.verbose("⚡ Interrupted during API call.")
	return &DoError{Failure: FailInterrupted, Err: err, Reason: ReasonCancelled, Route: l.route, Message: msg, Summary: msg}
}

// call issues the request on the streaming or blocking path
func (l *doLoop) call() (*Response, time.Time, error) {
	if err := checkStaleGiveup(l.session); err != nil {
		return nil, time.Time{}, err
	}
	if l.opts.Stream != nil && !l.session.DisableStreaming {
		return l.streamWithReconnects()
	}
	resp, err := l.client.Complete(l.ctx, l.req)
	return resp, time.Time{}, err
}

// PartialStreamError is a stream that died after visible deltas were delivered
type PartialStreamError struct {
	Err error
}

func (e *PartialStreamError) Error() string {
	return "stream failed after partial delivery: " + e.Err.Error()
}

func (e *PartialStreamError) Unwrap() error { return e.Err }

func streamRetries() int {
	raw := strings.TrimSpace(os.Getenv(StreamRetriesEnv))
	if raw == "" {
		return 2
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 2
	}
	return n
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

func isSSEConnectionError(apiErr *APIError) bool {
	if apiErr == nil || apiErr.StatusCode != 0 {
		return false
	}
	return anyIn(strings.ToLower(apiErr.Message+" "+errorText(apiErr)), sseConnPhrases)
}

func isStreamUnavailableError(err error) bool {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "stream") && strings.Contains(lower, "not supported") {
		return true
	}
	if strings.Contains(lower, "unexpected event order") {
		return true
	}
	return strings.Contains(lower, "invokemodelwithresponsestream") && (strings.Contains(lower, "accessdenied") || strings.Contains(lower, "not authorized"))
}

func isEmptyFrameError(apiErr *APIError) bool {
	return apiErr != nil && apiErr.Code == ProviderStreamEmptyFrameErrorCode
}

func streamsLlamaCpp(route Route) bool {
	return IsLlamaCppAlias(route.Provider)
}

func (l *doLoop) connectExhaustedNotice(err error, attempts int) string {
	host := hostOfBaseURL(l.route.BaseURL)
	if host == "" {
		host = "the endpoint"
	}
	plural := "s"
	if attempts == 1 {
		plural = ""
	}
	line := fmt.Sprintf("❌ Could not open a stream to %s after %d attempt%s", host, attempts, plural)
	size := 0
	if raw, marshalErr := json.Marshal(requestBody(l.req)); marshalErr == nil {
		size = len(raw)
	}
	if size <= 0 {
		return line + "; the endpoint looks unreachable — try again in a moment."
	}
	kb := int(math.Round(float64(size) / 1024))
	if kb < 1 {
		kb = 1
	}
	line += fmt.Sprintf(" (request %d KB)", kb)
	if size >= 256*1024 {
		return line + "; the endpoint or a proxy in front of it may reject requests this large."
	}
	return line + "; the endpoint looks unreachable — try again in a moment."
}

func (l *doLoop) bufferOnce(text string) {
	for _, existing := range l.session.retryStatusBuffer {
		if existing == text {
			return
		}
	}
	l.bufferStatus(text)
}

// maybeDisableStreaming flips the session to non streaming for failures re streaming can only repeat
func (l *doLoop) maybeDisableStreaming(err error, apiErr *APIError) {
	if streamsLlamaCpp(l.route) {
		return
	}
	if isEmptyFrameError(apiErr) {
		l.session.DisableStreaming = true
		l.log.Warn(fmt.Sprintf("Provider stream returned an empty SSE frame (keepalive, no payload) before any delta — switching %s/%s to non-streaming for this session.", orUnknown2(l.route.Provider), orUnknown2(l.route.Model)))
		if l.opts.Warning != nil {
			l.opts.Warning("⚠️ Provider stream returned an empty keepalive frame — retrying this turn without streaming (streaming stays off for this session).")
		}
		return
	}
	if !isStreamUnavailableError(err) {
		return
	}
	lower := strings.ToLower(err.Error())
	unsupported := strings.Contains(lower, "stream") && strings.Contains(lower, "not supported")
	if strings.Contains(lower, "unexpected event order") && !unsupported {
		if l.route.Mode != ModeAnthropicMessages || l.route.Provider == "bedrock" {
			return
		}
		l.session.DisableStreaming = true
		l.status("\n⚠  Provider sent Anthropic stream events out of order. Switching to non-streaming.\n")
		return
	}
	l.session.DisableStreaming = true
	if unsupported {
		l.status("\n⚠  Streaming is not supported for this model/provider. Switching to non-streaming.\n   To avoid this delay, set display.streaming: false in config.yaml\n")
	} else {
		l.status("\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream. Switching to non-streaming.\n   Grant that action to restore streaming output.\n")
	}
}

func orUnknown2(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// unmaskServerErrorWithNonStreaming re issues one blocking request when a 5xx killed the stream before any delta
func (l *doLoop) unmaskServerErrorWithNonStreaming(apiErr *APIError, deltasSent bool) (*Response, error, bool) {
	if l.ctx.Err() != nil || streamsLlamaCpp(l.route) {
		return nil, nil, false
	}
	if apiErr == nil || apiErr.StatusCode < 500 || deltasSent {
		return nil, nil, false
	}
	if l.route.Mode != "" && l.route.Mode != ModeChatCompletions {
		return nil, nil, false
	}
	now := l.now()
	if l.session.stream5xxProbed && now.Sub(l.session.stream5xxProbeAt) < stream5xxProbeWindow {
		return nil, nil, false
	}
	l.session.stream5xxProbed, l.session.stream5xxProbeAt = true, now
	probeReq := *l.req
	probeReq.Stream = false
	probe, err := l.client.Complete(l.ctx, &probeReq)
	if err != nil {
		if l.ctx.Err() != nil {
			return nil, err, true
		}
		probeErr := l.asAPIError(err)
		if probeErr.StatusCode > 0 && probeErr.StatusCode < 500 {
			l.log.Info(fmt.Sprintf("Non-streaming unmask probe surfaced the underlying error: %v", probeErr))
			return nil, probeErr, true
		}
		l.log.Info(fmt.Sprintf("Non-streaming unmask probe failed: %v", err))
		return nil, nil, false
	}
	l.log.Info(fmt.Sprintf("Streaming 5xx re-issued non-streaming successfully for %s/%s (not latched: the 5xx may be transient).", orUnknown2(l.route.Provider), orUnknown2(l.route.Model)))
	l.bufferStatus("⚠  Streaming failed with a provider server error; the non-streaming retry succeeded.")
	if err := l.replayResponse(probe); err != nil {
		l.log.Error(fmt.Sprintf("Non-streaming unmask probe response could not be replayed: %v", err))
		return nil, nil, false
	}
	return probe, nil, true
}

// replayResponse feeds a blocking response through the stream consumer as deltas
func (l *doLoop) replayResponse(resp *Response) error {
	if resp == nil || resp.Message == nil {
		return errors.New("empty probe response")
	}
	fn := l.opts.Stream
	if resp.Message.Reasoning != "" {
		if err := fn(StreamEvent{Kind: EventReasoningDelta, Text: resp.Message.Reasoning}); err != nil {
			return err
		}
	}
	if text := resp.Message.Text(); text != "" {
		if err := fn(StreamEvent{Kind: EventTextDelta, Text: text}); err != nil {
			return err
		}
	}
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		if err := fn(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return err
		}
	}
	usage := resp.Usage
	if err := fn(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return err
	}
	return fn(StreamEvent{Kind: EventDone})
}

// streamWithReconnects runs the stream attempt loop with transient reconnects before the main loop sees an error
func (l *doLoop) streamWithReconnects() (*Response, time.Time, error) {
	maxStreamRetries := streamRetries()
	compatRetries := 0
	attempt := -1
	var firstChunk time.Time
	for attempt < maxStreamRetries+compatRetries {
		attempt++
		if l.ctx.Err() != nil {
			return nil, firstChunk, l.ctx.Err()
		}
		deltasSent := false
		partialToolInFlight := false
		fn := func(ev StreamEvent) error {
			switch ev.Kind {
			case EventTextDelta, EventReasoningDelta:
				if ev.Text != "" {
					if firstChunk.IsZero() {
						firstChunk = l.now()
					}
					deltasSent = true
					l.session.ResetStaleStreak()
					if ev.Kind == EventTextDelta {
						l.streamedText += ev.Text
					}
				}
			case EventToolCallStart, EventToolCallDelta:
				if firstChunk.IsZero() {
					firstChunk = l.now()
				}
				partialToolInFlight = true
			case EventToolCallDone:
				partialToolInFlight = false
				deltasSent = true
				l.session.ResetStaleStreak()
			}
			return l.opts.Stream(ev)
		}
		resp, err := l.client.Stream(l.ctx, l.req, fn)
		if err == nil {
			return resp, firstChunk, nil
		}
		if l.ctx.Err() != nil {
			return nil, firstChunk, err
		}
		apiErr := l.asAPIError(err)
		var empty *EmptyStreamError
		isEmptyStream := errors.As(err, &empty)
		isTimeout := apiErr.Kind == ErrTimeout || TimeoutPhase(err) != ""
		isConnErr := apiErr.Kind == ErrNetwork && !isCertificateError(err)
		isParseErr := IsProviderStreamParseError(err) || apiErr.Code == ProviderStreamNonJSONErrorCode
		isSSEConn := !isTimeout && !isConnErr && isSSEConnectionError(apiErr)
		isTransient := isTimeout || isConnErr || isSSEConn || isParseErr
		if !deltasSent && !l.session.StreamOptionsUnsupported && rejectsStreamOptions(apiErr) {
			l.session.StreamOptionsUnsupported = true
			compatRetries = 1
			if l.req.Metadata == nil {
				l.req.Metadata = map[string]string{}
			}
			l.req.Metadata[StreamOptionsOffKey] = "off"
			l.log.Info(fmt.Sprintf("Endpoint rejected stream_options (HTTP %d); retrying without it for this session.", apiErr.StatusCode))
			continue
		}
		if deltasSent {
			if !(partialToolInFlight && isTransient && attempt < maxStreamRetries) {
				l.log.Warn(fmt.Sprintf("Streaming failed after partial delivery, not retrying: %v", err))
				return nil, firstChunk, &PartialStreamError{Err: err}
			}
			if l.opts.Warning != nil {
				l.opts.Warning("\n\n⚠ Connection dropped mid tool-call; reconnecting…\n\n")
			}
			if waitErr := l.retryAfterDrop(err, attempt, maxStreamRetries, true); waitErr != nil {
				return nil, firstChunk, waitErr
			}
			continue
		}
		if isTransient || isEmptyStream {
			if attempt < maxStreamRetries {
				if waitErr := l.retryAfterDrop(err, attempt, maxStreamRetries, false); waitErr != nil {
					return nil, firstChunk, waitErr
				}
				continue
			}
			l.log.Warn(fmt.Sprintf("Stream exhausted on attempt %d/%d. %s error_type=%T error=%s", maxStreamRetries+1, maxStreamRetries+1, l.logContext(), err, truncate(SummarizeAPIError(err, l.route.Provider), 240)))
			if isParseErr || isEmptyStream {
				what := "Provider returned an empty response stream after"
				if isParseErr {
					what = "Provider returned malformed streaming data after"
				}
				l.bufferStatus(fmt.Sprintf("❌ %s %d attempts. The provider may be experiencing issues — try again in a moment.", what, maxStreamRetries+1))
			} else {
				l.bufferOnce(l.connectExhaustedNotice(err, maxStreamRetries+1))
			}
			return nil, firstChunk, err
		}
		l.maybeDisableStreaming(err, apiErr)
		l.log.Error(fmt.Sprintf("Streaming failed before delivery: %v", err))
		if resp, probeErr, handled := l.unmaskServerErrorWithNonStreaming(apiErr, deltasSent); handled {
			if probeErr != nil {
				return nil, firstChunk, probeErr
			}
			return resp, firstChunk, nil
		}
		return nil, firstChunk, err
	}
	return nil, firstChunk, errors.New("stream attempt loop ended without a response")
}

// retryAfterDrop announces a stream drop and backs off between reconnects
func (l *doLoop) retryAfterDrop(err error, attempt, maxRetries int, midToolCall bool) error {
	kind := "drop"
	if midToolCall {
		kind = "drop mid tool-call"
	}
	l.log.Warn(fmt.Sprintf("Stream %s on attempt %d/%d — retrying. %s error_type=%T error=%s", kind, attempt+1, maxRetries+1, l.logContext(), err, truncate(SummarizeAPIError(err, l.route.Provider), 240)))
	provider := l.route.Provider
	if provider == "" {
		provider = "provider"
	}
	l.bufferStatus(fmt.Sprintf("⚠️ %s stream %s (%T) — attempt %d/%d dropped, reconnecting", provider, kind, err, attempt+1, maxRetries+1))
	l.activity(fmt.Sprintf("stream retry %d/%d after %T", attempt+1, maxRetries+1, err))
	return l.sleep(l.ctx, JitteredBackoff(attempt+1, BackoffOptions{BaseDelay: time.Second, MaxDelay: 4 * time.Second, JitterRatio: 0}))
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
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "nonetype") && strings.Contains(text, "not iterable") {
		return false
	}
	return true
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

// logAPIErrorAttempt logs one failed attempt and buffers the retry trace with the OpenRouter and prefix hints
func (l *doLoop) logAPIErrorAttempt(apiErr *APIError, elapsed time.Duration, retryable bool) (errorMsg, summary string) {
	errorMsg = strings.ToLower(apiErr.Error())
	summary = SummarizeAPIError(apiErr, l.route.Provider)
	attempt := fmt.Sprintf("attempt %d/%d", l.retryCount, l.maxRetries)
	if !retryable {
		attempt += ", not retryable"
	}
	l.log.Warn(fmt.Sprintf("API call failed (%s) error_type=%s %s summary=%s", attempt, errorTypeName(apiErr), l.logContext(), summary))
	l.bufferStatus(fmt.Sprintf("⚠️  %s%s failed: %s", strings.ToUpper(attempt[:1]), attempt[1:], summary))
	if l.opts.VerboseLogging {
		statusStr := ""
		if apiErr.StatusCode > 0 {
			statusStr = fmt.Sprintf(" [HTTP %d]", apiErr.StatusCode)
		}
		l.bufferStatus(fmt.Sprintf("   🔌 %s%s  Provider: %s  Model: %s", errorTypeName(apiErr), statusStr, l.route.Provider, l.route.Model))
		l.bufferStatus(fmt.Sprintf("   🌐 Endpoint: %s", l.route.BaseURL))
		if apiErr.StatusCode > 0 && apiErr.StatusCode < 500 && apiErr.Body != "" {
			l.bufferStatus(fmt.Sprintf("   📋 Details: %s", truncate(apiErr.Body, 300)))
		}
		l.bufferStatus(fmt.Sprintf("   ⏱️  Elapsed: %.2fs  Context: %d msgs, ~%s tokens", elapsed.Seconds(), len(l.req.Messages), formatThousands(l.opts.ApproxTokens)))
	}
	if l.isOpenRouterURL() && strings.Contains(errorMsg, "support tool use") {
		l.bufferStatus(fmt.Sprintf("   💡 No OpenRouter providers for %s support tool calling with your current settings.", l.route.Model))
		if only, ok := l.req.ExtraBody["provider"].(map[string]any); ok && only["only"] != nil {
			l.bufferStatus("      Your provider_routing.only restriction is filtering out tool-capable providers.")
			l.bufferStatus("      Try removing the restriction or adding providers that support tools for this model.")
		}
		l.bufferStatus(fmt.Sprintf("      Check which providers support tools: https://openrouter.ai/models/%s", l.route.Model))
	}
	if suggestion := l.missingVendorPrefixSuggestion(apiErr); suggestion != "" {
		l.bufferStatus(fmt.Sprintf("   💡 Model '%s' is not a valid id for provider %s — it is missing its vendor prefix.", l.route.Model, l.route.Provider))
		l.bufferStatus(fmt.Sprintf("      Did you mean '%s'?  Re-pick it with /model.", suggestion))
	}
	return errorMsg, summary
}

func formatThousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func (l *doLoop) missingVendorPrefixSuggestion(apiErr *APIError) string {
	if apiErr == nil || apiErr.StatusCode != 404 {
		return ""
	}
	return SuggestPrefixedModelID(l.route.Provider, l.route.Model)
}

// genuineNousRateLimit records an account level Nous 429 to the cross session breaker
func (l *doLoop) genuineNousRateLimit(apiErr *APIError, errorContext map[string]any, cl *Classified) bool {
	headers := errorHeaders(apiErr)
	anonymous := l.anonymous()
	var lastKnown *RateLimitState
	if l.opts.Credits != nil {
		lastKnown = l.opts.Credits.RateLimit()
	}
	genuine := (anonymous && IsLongWelcomeRateLimit(cl.Context)) || IsGenuineNousRateLimit(headers, lastKnown, l.now())
	if !genuine {
		l.log.Info("Nous 429 looks like upstream capacity (no exhausted bucket in headers or last-known state) -- not tripping cross-session breaker.")
		return false
	}
	merged := map[string]any{}
	for k, v := range errorContext {
		merged[k] = v
	}
	for k, v := range cl.Context {
		merged[k] = v
	}
	resetAt := time.Time{}
	if epoch, ok := ParseAbsoluteTimestamp(merged["reset_at"]); ok {
		resetAt = time.Unix(0, int64(epoch*1e9))
	}
	guard := &NousRateGuard{Home: l.opts.Home, Anonymous: anonymous, Now: l.now}
	if err := guard.Record(headers, resetAt, 300*time.Second); err != nil {
		l.log.Debug(fmt.Sprintf("nous rate guard record failed: %v", err))
	}
	return true
}

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

// handleError recovers from one failed attempt in the hermes order and reports whether the loop continues
func (l *doLoop) handleError(err error, start time.Time) (*DoError, bool, error) {
	if l.opts.RecoverBeforeClassification != nil && l.opts.RecoverBeforeClassification(l.ctx, l.st, err) {
		return nil, true, nil
	}
	apiErr := l.asAPIError(err)
	headers := errorHeaders(apiErr)
	if l.opts.Fast != nil && FastModeUnprovisioned(apiErr, l.req.ExtraBody, headers) && l.opts.Fast.MarkFastModeUnavailable(l.route.Model) {
		l.verbose(fmt.Sprintf("⚠️  Fast mode isn't available for %s on this Anthropic organization — using standard speed for this session, retrying...", l.route.Model))
		l.log.Warn(fmt.Sprintf("Fast mode: %s has a fast-mode limit of 0; standard speed for this session", l.route.Model))
		if l.req.ExtraBody != nil {
			delete(l.req.ExtraBody, "speed")
		}
		return nil, true, nil
	}
	errorContext := ExtractAPIErrorContext(apiErr)
	if (apiErr.StatusCode == 402 || apiErr.StatusCode == 403) && l.route.Provider == "nous" && l.opts.Credits != nil && ResponseIsUsageWindow(headers) {
		l.opts.Credits.CaptureCredits(headers, true)
	}
	cl := l.classify(apiErr)
	l.log.Debug(fmt.Sprintf("Error classified: reason=%s status=%d retryable=%t compress=%t rotate=%t fallback=%t", cl.Reason, cl.StatusCode, cl.Retryable, cl.ShouldCompress, cl.ShouldRotateCredential, cl.ShouldFallback))
	l.fireErrorHook(start, apiErr, cl)
	if l.opts.RecoverBeforeRotation != nil && l.opts.RecoverBeforeRotation(l.ctx, l.st, cl, apiErr) {
		return nil, true, nil
	}
	recoveredWithPool := false
	if l.opts.Rotator != nil {
		if next, ok := l.opts.Rotator.Rotate(l.ctx, l.route, apiErr); ok {
			if switchErr := l.switchRoute(next); switchErr != nil {
				return nil, false, switchErr
			}
			if l.chain != nil {
				l.chain.SetCurrent(next)
			}
			return nil, true, nil
		}
	}
	if l.opts.RecoverAfterRotation != nil && l.opts.RecoverAfterRotation(l.ctx, l.st, cl, apiErr) {
		return nil, true, nil
	}
	l.retryCount++
	l.st.RetryCount = l.retryCount
	elapsed := l.now().Sub(start)
	l.activity(fmt.Sprintf("API error recovery (attempt %d/%d)", l.retryCount, l.maxRetries))
	errorMsg, _ := l.logAPIErrorAttempt(apiErr, elapsed, cl.Retryable)
	if l.ctx.Err() != nil {
		msg := fmt.Sprintf("Operation interrupted: handling API error (%s: %s).", errorTypeName(apiErr), CleanErrorMessage(apiErr.Error()))
		l.verbose("⚡ Interrupt detected during error handling, aborting retries.")
		return &DoError{Failure: FailInterrupted, Err: l.ctx.Err(), APIError: apiErr, Classified: cl, Reason: ReasonCancelled, Route: l.route, Message: msg, Summary: msg}, false, nil
	}
	// route_classified_error
	isOutputCapError := IsOutputCapError(errorMsg)
	if !isOutputCapError {
		_, isOutputCapError = ParseAvailableOutputTokensFromError(errorMsg)
	}
	if overflowReasons[cl.Reason] && !l.opts.CompressionEnabled && !isOutputCapError {
		l.flushStatus()
		l.verbose("❌ The conversation is too long for the model and automatic shrinking is off (compression.enabled: false).",
			"   💡 Run /compress to shrink it now, /new to start fresh, pick a model with a bigger context window, or remove attachments.")
		l.log.Error(fmt.Sprintf("Context overflow (%s) with auto-compaction disabled — not compressing.", cl.Reason))
		return &DoError{Failure: FailCompactionDisabled, Err: apiErr, APIError: apiErr, Classified: cl, Reason: ReasonContextOverflow, Retryable: false, Route: l.route,
			Summary: SummarizeAPIError(apiErr, l.route.Provider), Message: SummarizeAPIError(apiErr, l.route.Provider)}, false, nil
	}
	if cl.Reason == ReasonLongContextTier && l.opts.OnLongContextTier != nil {
		l.compressionAttempts++
		l.st.CompressionAttempts = l.compressionAttempts
		v := l.opts.OnLongContextTier(l.ctx, l.st, cl, apiErr)
		if v.Err != nil {
			return &DoError{Failure: FailOverflow, Err: v.Err, APIError: apiErr, Classified: cl, Reason: cl.Reason, Route: l.route, Summary: v.Err.Error(), Message: v.Err.Error()}, false, nil
		}
		if v.Handled || v.Restart {
			l.st.RestartWithCompressedMessages = true
			return nil, true, nil
		}
	}
	isRateLimited := RateLimitFailoverReasons[cl.Reason]
	wrappedOutputCap, hasWrappedCap := 0, false
	if cl.Reason == ReasonRateLimit {
		wrappedOutputCap, hasWrappedCap = ParseAvailableOutputTokensFromError(errorMsg)
	}
	isTransportFailure := transportFailureReasons[cl.Reason]
	isZai := IsZAICodingOverload(l.route.BaseURL, l.route.Model, apiErr)
	if isZai && l.maxRetries < ZAICodingOverloadRetryCeiling() {
		l.maxRetries = ZAICodingOverloadRetryCeiling()
		l.st.MaxRetries = l.maxRetries
	}
	shouldFallback := ((isRateLimited && !hasWrappedCap) || (isTransportFailure && l.retryCount >= 2)) && !usageWindowOf(cl)
	if shouldFallback && l.hasPendingFallback() {
		isUpstream := cl.Reason == ReasonUpstreamRateLimit
		poolMayRecover := false
		if !isUpstream && l.chain != nil {
			poolMayRecover = PoolMayRecoverFromRateLimit(l.chain.Pool())
		}
		if !poolMayRecover {
			l.bufferStatus(eagerFallbackStatus(cl, isUpstream, isTransportFailure))
			if switched, fbErr := l.tryFallback(cl.Reason, resetAtOf(errorContext)); fbErr != nil {
				return nil, false, fbErr
			} else if switched {
				return nil, true, nil
			}
		}
	}
	if cl.IsAuth() && !l.st.AuthFailoverAttempted && l.hasPendingFallback() {
		l.st.AuthFailoverAttempted = true
		l.bufferStatus("🔐 Authentication failed and could not be refreshed — switching to fallback provider...")
		if switched, fbErr := l.tryFallback(cl.Reason, nil); fbErr != nil {
			return nil, false, fbErr
		} else if switched {
			return nil, true, nil
		}
	}
	if isRateLimited && l.route.Provider == "nous" && cl.Reason == ReasonRateLimit && !recoveredWithPool && l.genuineNousRateLimit(apiErr, errorContext, cl) {
		l.retryCount = l.maxRetries - 1
		if l.retryCount < 0 {
			l.retryCount = 0
		}
		return nil, true, nil
	}
	// recover_from_overflow
	isContextLengthError := false
	if l.opts.OnOverflow != nil {
		v := l.opts.OnOverflow(l.ctx, l.st, cl, apiErr, wrappedOutputCap)
		if v.Err != nil {
			return &DoError{Failure: FailOverflow, Err: v.Err, APIError: apiErr, Classified: cl, Reason: cl.Reason, Route: l.route, Summary: v.Err.Error(), Message: v.Err.Error()}, false, nil
		}
		isContextLengthError = v.IsContextLengthError
		l.compressionAttempts = l.st.CompressionAttempts
		if v.Handled || v.Restart {
			return nil, true, nil
		}
	}
	return l.settleUnrecovered(apiErr, cl, errorContext, errorMsg, isContextLengthError, isRateLimited, isZai)
}

// settleUnrecovered decides between a terminal client error, exhaustion handling, and the retry backoff
func (l *doLoop) settleUnrecovered(apiErr *APIError, cl *Classified, errorContext map[string]any, errorMsg string, isContextLengthError, isRateLimited, isZai bool) (*DoError, bool, error) {
	isLocalValidation := IsLocalValidationError(apiErr)
	shrinkSpent := cl.Reason == ReasonImageTooLarge && l.st.ImageShrinkRetryAttempted
	reasoningSpent := cl.Reason == ReasonReasoningMandatory && l.st.ReasoningMandatoryRetryAttempted
	isClientError := (isLocalValidation || shrinkSpent || reasoningSpent || (!cl.Retryable && !cl.ShouldCompress && !RetryableClientReasons[cl.Reason])) && !isContextLengthError
	if isClientError {
		var poolHas func(string) bool
		if l.chain != nil && l.chain.Pool() != nil {
			pool := l.chain.Pool()
			poolHas = pool.HasAvailable
		}
		l.cooldown.MarkEntitlementRejected(apiErr, l.route.Provider, l.route.Model, poolHas)
		isStale := l.opts.IsStaleCredentialError
		if isStale == nil {
			isStale = func(route Route, status int, message string) bool {
				return routeProfileIsStaleCredentialError(l.ctx, route, status, message)
			}
		}
		if !l.st.ProviderStaleRefreshAttempted && l.opts.RefreshStaleCredential != nil && isStale(l.route, apiErr.StatusCode, apiErr.Message) {
			l.st.ProviderStaleRefreshAttempted = true
			if notice, refreshed := l.opts.RefreshStaleCredential(l.ctx, l.st, apiErr); refreshed {
				if notice == "" {
					notice = fmt.Sprintf("%s credential renewed after a stale-credential error. Retrying request...", Label(l.route.Provider))
				}
				l.verbose("🔐 " + notice)
				l.retryCount = 0
				l.route, l.client, l.req = l.st.Route, l.st.Client, l.st.Request
				return nil, true, nil
			}
		}
		unclassifiedLocal := isLocalValidation && cl.Reason == ReasonUnknown
		if cl.ShouldFallback || unclassifiedLocal || shrinkSpent || reasoningSpent {
			if l.hasPendingFallback() {
				label, ok := nonretryableLabels[cl.Reason]
				if !ok {
					label = fmt.Sprintf("Non-retryable error (HTTP %d)", apiErr.StatusCode)
				}
				l.bufferStatus(fmt.Sprintf("⚠️ %s — trying fallback...", label))
			}
			if switched, fbErr := l.tryFallback(cl.Reason, resetAtOf(errorContext)); fbErr != nil {
				return nil, false, fbErr
			} else if switched {
				return nil, true, nil
			}
		}
		return l.nonretryableResult(apiErr, cl, errorContext), false, nil
	}
	attemptsMade := l.retryCount
	if attemptsMade > l.maxRetries {
		attemptsMade = l.maxRetries
	}
	if isRateLimited && l.retryCount < l.maxRetries && l.freeTierCooldownEndsTurn(apiErr) {
		l.log.Info(fmt.Sprintf("Free-tier cooldown outlasts the attended wait — ending retries after attempt %d", l.retryCount))
		l.retryCount = l.maxRetries
	}
	if l.retryCount >= l.maxRetries {
		if !l.st.PrimaryRecoveryAttempted {
			recovered, recErr := l.tryRecoverPrimaryTransport(apiErr)
			if recErr != nil {
				return nil, false, recErr
			}
			if recovered {
				l.st.PrimaryRecoveryAttempted = true
				l.retryCount = 0
				l.st.HasRetried429 = false
				if l.chain != nil {
					l.chain.ResetForFreshAttempt()
				}
				return nil, true, nil
			}
		}
		if l.hasPendingFallback() {
			l.bufferStatus(fmt.Sprintf("⚠️ Max retries (%d) exhausted — trying fallback...", l.maxRetries))
		}
		if switched, fbErr := l.tryFallback(cl.Reason, resetAtOf(errorContext)); fbErr != nil {
			return nil, false, fbErr
		} else if switched {
			return nil, true, nil
		}
		if de, applied, ladderErr := l.autoRecoverAfterExhaustion(apiErr, cl); ladderErr != nil {
			return nil, false, ladderErr
		} else if de != nil {
			return de, false, nil
		} else if applied {
			l.retryCount = 0
			return nil, true, nil
		}
		return l.exhaustedResult(apiErr, cl, errorContext, errorMsg, attemptsMade, isRateLimited), false, nil
	}
	waitTime := l.computeErrorBackoff(apiErr, isRateLimited, isZai)
	if de := l.interruptibleBackoffSleep(waitTime, "Interrupt detected during retry wait, aborting.",
		fmt.Sprintf("Operation interrupted: retrying API call after error (retry %d/%d).", l.retryCount, l.maxRetries),
		fmt.Sprintf("error retry backoff (%d/%d)", l.retryCount, l.maxRetries), apiErr, cl); de != nil {
		return de, false, nil
	}
	return nil, true, nil
}

// freeTierCooldownEndsTurn reports an attended free model session facing a cooldown longer than a person waits
func (l *doLoop) freeTierCooldownEndsTurn(apiErr *APIError) bool {
	if !(l.onFreeModel() && l.opts.Attended) {
		return false
	}
	cooldown, ok := ProviderRetryAfter(apiErr)
	return ok && cooldown > LiveRetryWaitCap
}

// tryRecoverPrimaryTransport rebuilds the primary client once for a transient transport error
func (l *doLoop) tryRecoverPrimaryTransport(apiErr *APIError) (bool, error) {
	if l.chain != nil && l.chain.Activated() {
		return false, nil
	}
	transient := apiErr.Kind == ErrNetwork || apiErr.Kind == ErrTimeout
	if !transient || l.isOpenRouterURL() || isCertificateError(apiErr) {
		return false, nil
	}
	provider := strings.ToLower(strings.TrimSpace(l.route.Provider))
	if (provider == "nous" || provider == "nous-portal" || provider == "nousresearch") && l.route.Mode != ModeAnthropicMessages {
		return false, nil
	}
	client, err := l.clientFor(l.route)
	if err != nil {
		l.log.Warn(fmt.Sprintf("Primary transport recovery failed: %v", err))
		return false, nil
	}
	l.client = client
	l.st.Client = client
	waitSeconds := 3 + l.retryCount
	if waitSeconds > 8 {
		waitSeconds = 8
	}
	l.status(fmt.Sprintf("🔁 Transient %s on %s — rebuilt client, waiting %ds before one last primary attempt.", errorTypeName(apiErr), l.route.Provider, waitSeconds))
	if err := l.sleep(l.ctx, time.Duration(waitSeconds)*time.Second); err != nil {
		return false, nil
	}
	return true, nil
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

// computeErrorBackoff picks and announces the wait before the next retry
func (l *doLoop) computeErrorBackoff(apiErr *APIError, isRateLimited, isZai bool) time.Duration {
	retryAfter, hasRetryAfter := ProviderRetryAfter(apiErr)
	if hasRetryAfter && retryAfter > RetryAfterCap {
		retryAfter = RetryAfterCap
	}
	waitTime := retryAfter
	if !hasRetryAfter {
		waitTime = JitteredBackoff(l.retryCount, BackoffOptions{BaseDelay: 2 * time.Second, MaxDelay: 60 * time.Second, JitterRatio: 0.5})
	}
	policy := ""
	adaptive := isRateLimited || isZai
	if adaptive && !hasRetryAfter {
		waitTime, policy = AdaptiveRateLimitBackoff(l.retryCount, l.route.BaseURL, l.route.Model, apiErr, waitTime)
	}
	reset := ""
	if adaptive {
		reset = ResetHint(apiErr, l.now())
	}
	freeBusy := isRateLimited && l.onFreeModel()
	waitReason := "Rate limited"
	if freeBusy {
		waitReason = "The free model is busy"
	} else if isZai && !isRateLimited {
		waitReason = "Provider overloaded"
	}
	var status string
	announceNow := false
	if adaptive {
		resetText := ""
		if reset != "" {
			resetText = " Resets in " + reset + "."
		}
		status = fmt.Sprintf("⏱️ %s.%s Waiting %.1fs (attempt %d/%d)%s...", waitReason, resetText, waitTime.Seconds(), l.retryCount+1, l.maxRetries, zaiPolicyNotes[policy])
		announceNow = policy == "zai_coding_overload_long"
	} else {
		status = fmt.Sprintf("⏳ Retrying in %.1fs (attempt %d/%d)...", waitTime.Seconds(), l.retryCount, l.maxRetries)
		announceNow = hasRetryAfter && retryAfter > LiveRetryWaitCap
	}
	if announceNow {
		l.status(status)
	} else {
		l.bufferStatus(status)
	}
	liveReason := "waiting on provider —"
	if reset != "" {
		liveReason = fmt.Sprintf("%s — resets in %s,", strings.ToLower(waitReason), reset)
	} else if freeBusy {
		liveReason = "the free model is busy —"
	}
	l.wait(fmt.Sprintf("⏳ %s retrying in %.0fs (attempt %d/%d)", liveReason, waitTime.Seconds(), l.retryCount, l.maxRetries))
	if policy == "" {
		policy = "default"
	}
	l.log.Warn(fmt.Sprintf("Retrying API call in %.1fs (attempt %d/%d) %s policy=%s error=%v", waitTime.Seconds(), l.retryCount, l.maxRetries, l.logContext(), policy, apiErr))
	return waitTime
}

// interruptibleBackoffSleep waits in slices touching activity every 30s and reports an interrupt as a terminal outcome
func (l *doLoop) interruptibleBackoffSleep(waitTime time.Duration, abortMessage, interruptText, activityLabel string, apiErr *APIError, cl *Classified) *DoError {
	end := l.now().Add(waitTime)
	for {
		remaining := end.Sub(l.now())
		if remaining <= 0 {
			break
		}
		slice := activityHeartbeat
		if remaining < slice {
			slice = remaining
		}
		if err := l.sleep(l.ctx, slice); err != nil {
			l.verbose("⚡ " + abortMessage)
			return &DoError{Failure: FailInterrupted, Err: err, APIError: apiErr, Classified: cl, Reason: ReasonCancelled, Route: l.route, Message: interruptText, Summary: interruptText}
		}
		if left := end.Sub(l.now()); left > 0 {
			l.activity(fmt.Sprintf("%s, %ds remaining", activityLabel, int(left.Seconds())))
		} else {
			break
		}
	}
	if l.opts.AfterWait != nil {
		l.opts.AfterWait(l.ctx)
	}
	return nil
}

func (l *doLoop) ladderEligible(cl *Classified) bool {
	if l.opts.AutoRecoveryCycles <= 0 || !ladderReasons[cl.Reason] {
		return false
	}
	streamed := l.streamedText
	if l.opts.StreamedText != nil {
		streamed = l.opts.StreamedText()
	}
	return !hasContentAfterThinkBlock(streamed)
}

var thinkBlockRe = regexp.MustCompile(`(?is)<(think|thinking|reasoning|thought)>.*?</(think|thinking|reasoning|thought)>`)

func hasContentAfterThinkBlock(content string) bool {
	if content == "" {
		return false
	}
	return strings.TrimSpace(thinkBlockRe.ReplaceAllString(content, "")) != ""
}

func (l *doLoop) ladderNotice(wait time.Duration, cycle, total int) string {
	hint, ok := ladderStopHints[strings.ToLower(l.opts.Platform)]
	if !ok {
		hint = ladderDefaultStopHint
	}
	text := fmt.Sprintf("⏳ Provider temporarily unavailable — retrying automatically in %.0fs (cycle %d/%d)", wait.Seconds(), cycle, total)
	if hint != "" {
		return text + "; " + hint
	}
	return text
}

// autoRecoverAfterExhaustion parks the turn on a transient outage for one jittered cycle when no fallback remains
func (l *doLoop) autoRecoverAfterExhaustion(apiErr *APIError, cl *Classified) (*DoError, bool, error) {
	if !l.ladderEligible(cl) {
		return nil, false, nil
	}
	total := l.opts.AutoRecoveryCycles
	if l.st.AutoRecoveryCyclesUsed >= total {
		l.status(fmt.Sprintf("⏳ Automatic recovery gave up after %d cycles — the provider is still unavailable.", total))
		return nil, false, nil
	}
	cycle := l.st.AutoRecoveryCyclesUsed + 1
	l.st.AutoRecoveryCyclesUsed = cycle
	wait, ok := ProviderRetryAfter(apiErr)
	if ok {
		if wait > 120*time.Second {
			wait = 120 * time.Second
		}
	} else {
		wait = JitteredBackoff(cycle, BackoffOptions{BaseDelay: 15 * time.Second, MaxDelay: 60 * time.Second, JitterRatio: 0.2})
	}
	notice := l.ladderNotice(wait, cycle, total)
	l.status(notice)
	l.wait(notice)
	l.log.Warn(fmt.Sprintf("Provider unavailable (%s) — auto-recovery cycle %d/%d, retrying in %.0fs %s", cl.Reason, cycle, total, wait.Seconds(), l.logContext()))
	if de := l.interruptibleBackoffSleep(wait, "Interrupt detected during automatic recovery wait, aborting.",
		fmt.Sprintf("Operation interrupted: waiting for the provider to recover (cycle %d/%d).", cycle, total),
		fmt.Sprintf("auto-recovery wait (%d/%d)", cycle, total), apiErr, cl); de != nil {
		return de, false, nil
	}
	return nil, true, nil
}

func welcomeTierGuidance(cl *Classified, model string, inChat, door bool) string {
	if cl == nil || cl.Context == nil {
		return ""
	}
	if refusal, ok := cl.Context["welcome_refusal"].(WelcomeRefusal); ok {
		return WelcomeRefusalCopy(refusal, model, inChat, door)
	}
	if refusal, ok := cl.Context["welcome_refusal"].(map[string]any); ok {
		parsed, _ := ParseWelcomeRefusal(map[string]any{"error": refusal})
		return WelcomeRefusalCopy(parsed, model, inChat, door)
	}
	if route, ok := cl.Context["welcome_route"].(string); ok && route != "" {
		return WelcomeRouteRefusalCopy(route, inChat, door)
	}
	return ""
}

var welcomeSurfaceKinds = map[string]string{"rate_limited": "rate_limited", "at_capacity": "at_capacity", "admission_closed": "at_capacity", "model_not_free": "model_not_free", "feature_not_free": "model_not_free"}

func welcomeSurfaceKind(cl *Classified) string {
	if cl == nil || cl.Context == nil {
		return ""
	}
	switch refusal := cl.Context["welcome_refusal"].(type) {
	case WelcomeRefusal:
		if kind, ok := welcomeSurfaceKinds[refusal.Reason]; ok {
			return kind
		}
		return "refused"
	case map[string]any:
		if kind, ok := welcomeSurfaceKinds[stringField(refusal, "reason")]; ok {
			return kind
		}
		return "refused"
	}
	route, _ := cl.Context["welcome_route"].(string)
	switch route {
	case "tier_disabled":
		return "disabled"
	case "session_expired":
		return "outage"
	case "named_on_welcome_host", "":
		return ""
	}
	return "route"
}

func (l *doLoop) welcomeOutageCopy(cl *Classified) string {
	if l.onFreeModel() && (cl.Reason == ReasonTimeout || cl.Reason == ReasonOverloaded || cl.Reason == ReasonServerError) {
		return FreeTierOutageCopy
	}
	return ""
}

func (l *doLoop) stampLimitReset(de *DoError, apiErr *APIError, cl *Classified) {
	resetAt, ok := cl.Context["reset_at"]
	if !ok {
		resetAt, ok = ExtractAPIErrorContext(apiErr)["reset_at"]
	}
	if !ok {
		return
	}
	if epoch, parsed := ParseAbsoluteTimestamp(resetAt); parsed {
		de.ResetAt, de.HasResetAt = epoch, true
	}
}

// nonretryableAuthGuidance prints the actionable lines for a terminal auth or billing error
func (l *doLoop) nonretryableAuthGuidance(cl *Classified, status int) []string {
	if usageWindowOf(cl) {
		return nil
	}
	var lines []string
	emit := func(ls ...string) { lines = append(lines, ls...) }
	provider := l.route.Provider
	if cl.Reason == ReasonBilling && PrintBillingOrEntitlementGuidance(emit, "model access", provider, l.route.BaseURL, l.route.Model, cl.BillingUnverified()) {
		return lines
	}
	if provider == "nous" && PrintNousEntitlementGuidance(emit, "Nous model access") {
		return lines
	}
	if (provider == "openai-codex" || provider == "xai-oauth" || provider == "nous") && status == 401 {
		switch provider {
		case "openai-codex":
			emit("   💡 Codex OAuth token was rejected (HTTP 401). Your token may have been",
				"      refreshed by another client (Codex CLI, VS Code) or another icebath profile.",
				fmt.Sprintf("      Sign this profile in again: `%s`", OAuthReloginCommand(provider)))
		case "xai-oauth":
			emit("   💡 xAI OAuth token was rejected (HTTP 401). To fix:",
				"      re-authenticate with xAI Grok OAuth (SuperGrok / Premium+) from `icebath model`.")
		default:
			emit("   💡 Nous Portal OAuth token was rejected (HTTP 401). Your token may be",
				"      expired, revoked, or your account may be out of credits. To fix:",
				"      1. Re-authenticate: icebath portal",
				"      2. Check your portal account: https://portal.nousresearch.com")
			if strings.HasSuffix(l.route.Model, ":free") {
				emit(fmt.Sprintf("      ⚠️  Note: `%s` looks like an OpenRouter slug (`:free` suffix).", l.route.Model),
					"         Nous Portal won't recognize that model name. Either switch to a",
					fmt.Sprintf("         Nous catalog model, or run `/model openrouter:%s` to use OpenRouter.", l.route.Model))
			}
		}
		return lines
	}
	if status == 401 && PluginRefreshHookExists(provider) {
		emit(fmt.Sprintf("   💡 %s sign-in was rejected (HTTP 401). Sign in again: `%s`", provider, OAuthReloginCommand(provider)))
		return lines
	}
	emit("   💡 Your API key was rejected by the provider. Check:",
		"      • Is the key valid? Run: icebath setup",
		fmt.Sprintf("      • Does your account have access to %s?", l.route.Model))
	if HostMatches(l.route.BaseURL, "openrouter.ai") {
		emit("      • Check credits: https://openrouter.ai/settings/credits")
	}
	return lines
}

// nonretryableResult is the terminal outcome of a 4xx once fallback is exhausted
func (l *doLoop) nonretryableResult(apiErr *APIError, cl *Classified, errorContext map[string]any) *DoError {
	if l.opts.DebugDump != nil {
		l.opts.DebugDump(l.req, "non_retryable_client_error", apiErr)
	}
	l.flushStatus()
	summary := SummarizeAPIError(apiErr, l.route.Provider)
	plabel := Label(l.route.Provider)
	label, ok := nonretryableLabels[cl.Reason]
	if !ok {
		label = fmt.Sprintf("%s rejected the request and retrying won't help", plabel)
	}
	l.status(fmt.Sprintf("❌ %s: %s", label, summary))
	var guidance []string
	if l.opts.VerboseLogging {
		guidance = append(guidance, fmt.Sprintf("   🔌 Provider: %s  Model: %s  (HTTP %d)", l.route.Provider, l.route.Model, apiErr.StatusCode), fmt.Sprintf("   🌐 Endpoint: %s", l.route.BaseURL))
	}
	welcomeHint := welcomeTierGuidance(cl, l.route.Model, false, true)
	prefixSuggestion := l.missingVendorPrefixSuggestion(apiErr)
	if moved := HostInstallHint(l.route.BaseURL); moved != "" {
		guidance = append(guidance, "   💡 "+moved)
	}
	switch {
	case welcomeHint != "":
		guidance = append(guidance, "   💡 "+welcomeHint)
	case cl.IsAuth() || cl.Reason == ReasonBilling:
		guidance = append(guidance, l.nonretryableAuthGuidance(cl, apiErr.StatusCode)...)
	case cl.Reason == ReasonModelNotFound:
		guidance = append(guidance, fmt.Sprintf("   💡 Model '%s' isn't available on %s. Pick another with /model.", l.route.Model, plabel))
		if prefixSuggestion != "" {
			guidance = append(guidance, fmt.Sprintf("      Did you mean '%s'? It looks like the vendor prefix is missing.", prefixSuggestion))
		}
	default:
		if _, labelled := nonretryableLabels[cl.Reason]; !labelled {
			guidance = append(guidance, fmt.Sprintf("   💡 Fix: pick another model (/model), or check `%s/logs/agent.log`.", home.Display(l.opts.Home)))
		}
	}
	if cl.Reason == ReasonUpstreamBlocked {
		guidance = append(guidance,
			"   💡 The endpoint's firewall/CDN blocked the request before it reached the model — your key",
			"      and model access are probably fine. Relays often reject the SDK's default User-Agent:",
			"      set `extra_headers: {User-Agent: HermesAgent/1.0}` on the custom_providers entry,",
			"      or check the proxy/WAF rules and your network.")
	}
	if cl.Reason == ReasonContentPolicyBlocked {
		guidance = append(guidance, "   💡 "+ContentPolicyNextSteps, "      To route future blocks to another provider automatically: icebath fallback add")
	}
	if cl.Reason == ReasonSSLCertVerification {
		guidance = append(guidance,
			"   💡 Hermes couldn't verify the provider's security certificate. This fails the same",
			"      way on every retry — fix the environment, then try again:",
			"      • Corporate TLS-inspecting proxy? Ask your administrator to install",
			"        its root certificate in the operating system trust store.",
			"      • Missing/stale system CA store? Refresh the OS certificate store.",
			"        A provider-specific CA can also be configured with ssl_ca_cert.",
			"      • Self-signed local endpoint (llama.cpp, LM Studio, vLLM)? Use http://",
			"        for localhost, or add the server's cert to your trust store.")
	}
	if len(guidance) > 0 {
		l.verbose(guidance...)
	}
	l.log.Error(fmt.Sprintf("Non-retryable client error: %v", apiErr))
	de := &DoError{Failure: FailNonRetryable, Err: apiErr, APIError: apiErr, Classified: cl, Summary: summary, Attempts: l.retryCount, Reason: cl.Reason,
		Retryable: cl.Retryable, BillingUnverified: cl.BillingUnverified(), PrefixSuggestion: prefixSuggestion, WelcomeHint: welcomeHint, UsageWindow: usageWindowOf(cl),
		Guidance: guidance, Route: l.route, Message: summary}
	if welcomeHint != "" {
		de.Message = welcomeTierGuidance(cl, l.route.Model, true, true)
		de.FreeTierKind = welcomeSurfaceKind(cl)
	}
	l.stampLimitReset(de, apiErr, cl)
	return de
}

// exhaustedResult is the terminal outcome once retries, transport recovery, and fallback all failed
func (l *doLoop) exhaustedResult(apiErr *APIError, cl *Classified, errorContext map[string]any, errorMsg string, attempts int, isRateLimited bool) *DoError {
	l.flushStatus()
	summary := SummarizeAPIError(apiErr, l.route.Provider)
	isBilling := cl.Reason == ReasonBilling
	var guidance []string
	reset := ""
	switch {
	case isBilling:
		if cl.BillingUnverified() {
			l.status(fmt.Sprintf("❌ Provider reported usage/credit exhaustion (unverified — may be a content-filter rejection) — %s", summary))
		} else {
			l.status(fmt.Sprintf("❌ Billing or credits exhausted — %s", summary))
		}
		if !usageWindowOf(cl) {
			emit := func(ls ...string) { guidance = append(guidance, ls...) }
			PrintBillingOrEntitlementGuidance(emit, "model access", l.route.Provider, l.route.BaseURL, l.route.Model, cl.BillingUnverified())
		}
	case isRateLimited:
		reset = ResetHint(apiErr, l.now())
		noun := "retries"
		if attempts == 1 {
			noun = "attempt"
		}
		resetText := ""
		if reset != "" {
			resetText = fmt.Sprintf(" (resets in %s)", reset)
		}
		l.status(fmt.Sprintf("❌ Rate limited after %d %s — %s%s", attempts, noun, summary, resetText))
	default:
		l.status(fmt.Sprintf("❌ API failed after %d retries — %s", attempts, summary))
	}
	guidance = append(guidance, fmt.Sprintf("   💀 Final error: %s", summary))
	welcomeHint := welcomeTierGuidance(cl, l.route.Model, false, true)
	if welcomeHint != "" {
		guidance = append(guidance, "   💡 "+welcomeHint)
	}
	isStreamDrop := apiErr.StatusCode == 0 && anyIn(errorMsg, streamDropMarkers)
	if isStreamDrop {
		guidance = append(guidance,
			"   💡 The provider's stream connection keeps dropping. This often happens when the model tries to write a very large file in a single tool call.",
			"      Try asking the model to use execute_code with Python's open() for large files, or to write the file in smaller sections.")
	}
	_, reasoningModel := ReasoningStaleTimeoutFloor(l.route.Model)
	isThinkingTimeout := cl.Reason == ReasonTimeout && reasoningModel && anyIn(strings.ToLower(errorMsg), thinkingTimeoutMarkers)
	if isThinkingTimeout {
		guidance = append(guidance, "   💡 "+strings.TrimSpace(ThinkingTimeoutGuidance(l.route.Provider, l.route.Model, l.opts.Home)))
	}
	l.verbose(guidance...)
	l.log.Error(fmt.Sprintf("API call failed after %d retries. %s | provider=%s model=%s msgs=%d tokens=~%s", attempts, summary, l.route.Provider, l.route.Model, len(l.req.Messages), formatThousands(l.opts.ApproxTokens)))
	if l.opts.DebugDump != nil {
		l.opts.DebugDump(l.req, "max_retries_exhausted", apiErr)
	}
	de := &DoError{Failure: FailExhausted, Err: apiErr, APIError: apiErr, Classified: cl, Summary: summary, Attempts: attempts, Reason: cl.Reason, Retryable: cl.Retryable,
		BillingUnverified: cl.BillingUnverified(), IsRateLimited: isRateLimited, IsStreamDrop: isStreamDrop, IsThinkingTimeout: isThinkingTimeout, ResetHint: reset,
		WelcomeHint: welcomeHint, UsageWindow: usageWindowOf(cl), Guidance: guidance, Route: l.route, Message: summary}
	if welcomeHint != "" {
		de.Message = welcomeTierGuidance(cl, l.route.Model, true, true)
		de.FreeTierKind = welcomeSurfaceKind(cl)
	} else if outage := l.welcomeOutageCopy(cl); outage != "" {
		de.Message, de.FreeTierKind = outage, "outage"
	}
	l.stampLimitReset(de, apiErr, cl)
	return de
}

// ThinkingTimeoutGuidance is the user facing fix list for a reasoning model that timed out before writing
func ThinkingTimeoutGuidance(provider, model, homeDir string) string {
	return fmt.Sprintf("%s was thinking for so long that the connection timed out before it wrote anything (common for reasoning models behind cloud gateways such as NVIDIA NIM, OpenAI, Anthropic, DeepSeek). Easiest fixes: `/reasoning low`, or switch to a faster model with /model. Advanced: set `providers.%s.models.%s.stale_timeout_seconds: 900` in `%s/config.yaml` to allow a longer wait.",
		model, provider, model, home.Display(homeDir))
}
