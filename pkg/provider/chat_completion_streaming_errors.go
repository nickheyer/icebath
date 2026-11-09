package provider

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One non streaming unmask probe per window across the agent's streaming calls
const stream5xxProbeWindowSeconds = 60.0

// Provider stream error carrying the SSE text the SDK JSON decoder rejected, an empty frame under its own code
func providerStreamErrorFromJSONDecodeError(err error, resp *http.Response) error {
	doc := ""
	var te *AuxTransportError
	if errors.As(err, &te) {
		doc = te.Doc
	}
	rawText := pystr.Strip(doc)
	var headers http.Header
	if resp != nil {
		headers = resp.Header
	}
	if rawText == "" {
		return &ProviderStreamError{Body: providerErrorBody(map[string]any{
			"code":    ProviderStreamEmptyFrameErrorCode,
			"message": "Provider stream returned an empty SSE data frame (keepalive with no payload).",
		}, 0), Headers: headers}
	}
	safe := tools.RedactSensitiveTextWith(nil, state.SanitizeSurrogates(rawText), tools.RedactOptions{Force: true})
	safe = pystr.Slice(safe, 0, providerStreamErrorTextLimit)
	message := safe
	if message == "" {
		message = "Provider stream returned non-JSON SSE data."
	}
	return &ProviderStreamError{Body: providerErrorBody(map[string]any{"code": ProviderStreamNonJSONErrorCode, "message": message}, 0),
		RawText: safe, Headers: headers}
}

// AnthropicConnectionError is the Anthropic SDK APIConnectionError wrapping a request that never got a response
type AnthropicConnectionError struct{ Err error }

func (e *AnthropicConnectionError) Error() string {
	if e.Err != nil && strings.Contains(strings.ToLower(e.Err.Error()), "timeout") {
		return "Request timed out."
	}
	return "Connection error."
}

func (e *AnthropicConnectionError) Unwrap() error { return e.Err }

// Status code on the error attribute itself, zero when it carries none
func streamErrorStatusAttr(err error) int {
	switch e := err.(type) {
	case *AuxSDKError:
		return e.StatusCode
	case *APIError:
		return e.StatusCode
	case *ProviderStreamError:
		return e.StatusCode
	}
	return 0
}

// HTTP status from the error or its cause chain, a numeric body code when none carries one
func streamErrorStatus(err error) (int, bool) {
	if status := auxExcHTTPStatus(err); status != 0 {
		return status, true
	}
	var pse *ProviderStreamError
	if errors.As(err, &pse) {
		if pse.StatusCode != 0 {
			return pse.StatusCode, true
		}
		if status := statusCodeFromBody(pse.Body); status != 0 {
			return status, true
		}
	}
	var sdk *AuxSDKError
	if errors.As(err, &sdk) {
		if body, ok := sdk.Body.(map[string]any); ok {
			if status := statusCodeFromBody(body); status != 0 {
				return status, true
			}
		}
	}
	return 0, false
}

// Timeout and connection drop classes the retry loop treats as transient
func streamErrorTransport(err error) (timeout, conn bool) {
	if _, ok := err.(*StreamLifetimeExceededError); ok {
		return true, false
	}
	if te, ok := err.(*AuxTransportError); ok {
		switch te.Class {
		case "ReadTimeout", "ConnectTimeout", "PoolTimeout":
			return true, false
		case "ConnectError", "ReadError", "RemoteProtocolError":
			return false, true
		}
	}
	if _, ok := err.(*AnthropicConnectionError); ok {
		return false, true
	}
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE, syscall.ESHUTDOWN} {
		if errors.Is(err, errno) {
			return false, true
		}
	}
	return false, false
}

// OpenAI SDK error without a status whose text names a dropped SSE connection
func streamIsSSEConnectionError(err error) bool {
	sdk, ok := err.(*AuxSDKError)
	if !ok || sdk.StatusCode != 0 {
		return false
	}
	return anyIn(strings.ToLower(err.Error()), sseConnPhrases)
}

// A 400 or 422 whose body names stream_options as an unknown or extra field
func streamRejectsStreamOptions(err error) bool {
	status := streamErrorStatusAttr(err)
	if status != 400 && status != 422 {
		return false
	}
	body := ""
	switch e := err.(type) {
	case *AuxSDKError:
		if pyrepr.Truthy(e.Body) {
			body = pyrepr.Str(e.Body)
		}
	case *APIError:
		body = e.Body
	case *ProviderStreamError:
		if len(e.Body) > 0 {
			body = pyrepr.Str(e.Body)
		}
	}
	text := strings.ToLower(body + " " + err.Error())
	return strings.Contains(text, "stream_options") && anyIn(text, []string{"extra", "not supported", "unrecognized", "unexpected", "unknown"})
}

var waitStreamRetryBackoffFn = waitStreamRetryBackoff

// Sleeps the backoff in 100ms steps, returning as soon as the agent is interrupted
func waitStreamRetryBackoff(agent ChatCompletionAgent, delay time.Duration) {
	deadline := time.Now().Add(max(delay, 0))
	for !agent.InterruptRequested() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		time.Sleep(min(100*time.Millisecond, remaining))
	}
}

// Turns off eager tool input streaming on every tool when the endpoint gets the fine grained streaming beta
func bufferAnthropicToolInput(apiKwargs map[string]any, baseURL string) {
	found := false
	for _, beta := range anthCommonBetasForBaseURL(baseURL, false) {
		found = found || beta == anthToolStreamingBeta
	}
	if !found {
		return
	}
	toolList, _ := apiKwargs["tools"].([]any)
	for _, raw := range toolList {
		if tool, ok := raw.(map[string]any); ok {
			tool["eager_input_streaming"] = false
		}
	}
}

// Classifies a failed attempt, true to retry, false to stop with the error recorded
func streamingCallHandleError(c *streamingCall, err error, attempt, maxRetries int) bool {
	if c.requestCancelled.Load() {
		streamingCallLog.Debug(fmt.Sprintf("Streaming worker caught %s after request cancellation — exiting without retry.", pyrepr.TypeName(err)))
		return false
	}
	err = c.asLifetimeError(err)
	isTimeout, isConn := streamErrorTransport(err)
	isParse := c.agent.APIMode() == string(ModeAnthropicMessages) && IsProviderStreamParseError(err)
	_, isEmpty := err.(*EmptyStreamError)
	isSSEConn := !isTimeout && !isConn && streamIsSSEConnectionError(err)
	isTransient := isTimeout || isConn || isSSEConn || isParse
	session := c.agent.RetrySession()
	if !c.deltasWereSent.Load() && !session.StreamOptionsUnsupported && streamRejectsStreamOptions(err) {
		session.StreamOptionsUnsupported = true
		c.mu.Lock()
		c.compatRetries = 1
		c.mu.Unlock()
		statusText := "None"
		if status := streamErrorStatusAttr(err); status != 0 {
			statusText = fmt.Sprint(status)
		}
		streamingCallLog.Info(fmt.Sprintf("Endpoint rejected stream_options (HTTP %s); retrying without it for this session.", statusText))
		c.cancelCurrentStreamAttempt("stream_options_rejected_retry")
		_ = c.clients.closeOnce("stream_options_rejected_retry", c.worker)
		return true
	}
	partialToolInFlight := c.forgetInvisibleDeltas(err)
	if c.deltasWereSent.Load() {
		if !(partialToolInFlight && isTransient && attempt < maxRetries) {
			streamingCallLog.Warn(fmt.Sprintf("Streaming failed after partial delivery, not retrying: %v", err))
			c.setResult(nil, err)
			return false
		}
		if c.agent.WarningPresentationEnabled() {
			quietCall(func() { c.agent.FireStreamDelta("\n\n⚠ Connection dropped mid tool-call; reconnecting…\n\n") })
		}
		quietCall(c.agent.ResetStreamDeliveryTracking)
		c.deltasWereSent.Store(false)
		c.firstDeltaFired.Store(false)
		c.retryAfterDrop(err, attempt, maxRetries, true, "stream_mid_tool_retry_cleanup")
		return true
	}
	if isTransient || isEmpty {
		if attempt < maxRetries {
			c.retryAfterDrop(err, attempt, maxRetries, false, "stream_retry_cleanup")
			return true
		}
		LogStreamRetry(c.agent, "exhausted", err, maxRetries+1, maxRetries+1, false, c.currentDiag())
		if isParse || isEmpty {
			what := "Provider returned an empty response stream after"
			if isParse {
				what = "Provider returned malformed streaming data after"
			}
			c.agent.BufferDiagnosticStatus(fmt.Sprintf("❌ %s %d attempts. The provider may be experiencing issues — try again in a moment.", what, maxRetries+1))
		} else {
			BufferConnectExhaustedNotice(c.agent, err, maxRetries+1, c.agent.BaseURL())
		}
	} else {
		c.maybeDisableStreaming(err)
		streamingCallLog.Error(fmt.Sprintf("Streaming failed before delivery: %v", err), "exc_info", err)
		if c.unmaskServerErrorWithNonstreaming(err) {
			return false
		}
	}
	c.setResult(nil, err)
	return false
}

func (c *streamingCall) currentDiag() *StreamDiag {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diag
}

// Reports the drop, tears down the request local client and waits the interruptible reconnect backoff
func (c *streamingCall) retryAfterDrop(err error, attempt, maxRetries int, midToolCall bool, reason string) {
	EmitStreamDrop(c.agent, err, attempt+1, maxRetries+1, midToolCall, c.currentDiag())
	if c.agent.APIMode() == string(ModeAnthropicMessages) && IsProviderStreamParseError(err) {
		bufferAnthropicToolInput(c.apiKwargs, c.agent.AnthropicBaseURL())
	}
	c.cancelCurrentStreamAttempt(reason)
	_ = c.clients.closeOnce(reason, c.worker)
	waitStreamRetryBackoffFn(c.agent, JitteredBackoff(attempt+1, BackoffOptions{BaseDelay: time.Second, MaxDelay: 4 * time.Second, JitterRatio: 0}))
	c.touchChunkClock()
}

func orUnknownName(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Flips the session to non streaming for failures re streaming can only repeat
func (c *streamingCall) maybeDisableStreaming(err error) {
	if streamsLlamaCpp(Route{Provider: c.agent.Provider(), BaseURL: c.agent.BaseURL(), Model: c.agent.Model()}) {
		return
	}
	if isProviderStreamEmptyFrameError(err) {
		c.agent.DisableStreaming()
		streamingCallLog.Warn(fmt.Sprintf("Provider stream returned an empty SSE frame (keepalive, no payload) before any delta — switching %s/%s to non-streaming for this session.",
			orUnknownName(c.agent.Provider()), orUnknownName(c.agent.Model())))
		c.agent.EmitWarning("⚠️ Provider stream returned an empty keepalive frame — retrying this turn without streaming (streaming stays off for this session).")
		return
	}
	if !isStreamUnavailableError(err) {
		return
	}
	lower := strings.ToLower(err.Error())
	unsupported := strings.Contains(lower, "stream") && strings.Contains(lower, "not supported")
	if strings.Contains(lower, "unexpected event order") && !unsupported {
		if c.agent.APIMode() != string(ModeAnthropicMessages) || c.agent.Provider() == "bedrock" {
			return
		}
		c.agent.DisableStreaming()
		c.agent.SafePrint("\n⚠  Provider sent Anthropic stream events out of order. Switching to non-streaming.\n", true)
		return
	}
	c.agent.DisableStreaming()
	if unsupported {
		c.agent.SafePrint("\n⚠  Streaming is not supported for this model/provider. Switching to non-streaming.\n   To avoid this delay, set display.streaming: false in config.yaml\n", true)
		return
	}
	c.agent.SafePrint("\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream. Switching to non-streaming.\n   Grant that action to restore streaming output.\n", true)
}

// One non streaming re-issue after a 5xx killed the stream before any delta, true when it set the outcome
func (c *streamingCall) unmaskServerErrorWithNonstreaming(err error) bool {
	if c.agent.InterruptRequested() {
		return false
	}
	if streamsLlamaCpp(Route{Provider: c.agent.Provider(), BaseURL: c.agent.BaseURL(), Model: c.agent.Model()}) {
		return false
	}
	status, ok := streamErrorStatus(err)
	if !ok || status < 500 || c.deltasWereSent.Load() {
		return false
	}
	if mode := c.agent.APIMode(); mode != "" && mode != string(ModeChatCompletions) {
		return false
	}
	session := c.agent.RetrySession()
	now := time.Now()
	if session.stream5xxProbed && now.Sub(session.stream5xxProbeAt).Seconds() < stream5xxProbeWindowSeconds {
		return false
	}
	session.stream5xxProbed, session.stream5xxProbeAt = true, now
	probeKwargs := make(map[string]any, len(c.apiKwargs))
	for k, v := range c.apiKwargs {
		if k != "stream" && k != "stream_options" {
			probeKwargs[k] = v
		}
	}
	c.mu.Lock()
	staleTimeout := c.staleTimeout
	c.staleTimeout = math.Inf(1)
	c.mu.Unlock()
	probe, probeErr := c.agent.InterruptibleAPICall(c.ctx, probeKwargs)
	c.mu.Lock()
	c.staleTimeout = staleTimeout
	c.mu.Unlock()
	if probeErr != nil {
		var interrupted *InterruptedError
		if errors.As(probeErr, &interrupted) {
			c.setResult(nil, probeErr)
			return true
		}
		if probeStatus, ok := streamErrorStatus(probeErr); ok && probeStatus < 500 {
			streamingCallLog.Info(fmt.Sprintf("Non-streaming unmask probe surfaced the underlying error: %v", probeErr))
			c.setResult(nil, probeErr)
			return true
		}
		streamingCallLog.Info(fmt.Sprintf("Non-streaming unmask probe failed: %v", probeErr))
		return false
	}
	streamingCallLog.Info(fmt.Sprintf("Streaming 5xx re-issued non-streaming successfully for %s/%s (not latched: the 5xx may be transient).",
		orUnknownName(c.agent.Provider()), orUnknownName(c.agent.Model())))
	quietCall(func() {
		c.agent.BufferStatus("⚠  Streaming failed with a provider server error; the non-streaming retry succeeded.")
	})
	replayed, replayErr := WithStreamEmitters(c.agent, func() (any, error) { return c.replayFinalResponse(probe), nil })
	if replayErr != nil {
		streamingCallLog.Error(fmt.Sprintf("Non-streaming unmask probe response could not be replayed: %v", replayErr), "exc_info", replayErr)
		return false
	}
	c.setResult(replayed, nil)
	return true
}
