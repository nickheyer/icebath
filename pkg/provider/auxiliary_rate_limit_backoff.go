package provider

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	auxRateLimitRetries       = 1
	auxRateLimitBackoffBase   = time.Second
	auxRateLimitBackoffCap    = 4 * time.Second
	auxRateLimitRetryAfterCap = 5 * time.Second
)

// Provider declared positive cooldown from a Retry-After header, a retry_after body field, or Bedrock response headers
func auxRateLimitRetryAfter(err error) (time.Duration, bool) {
	value, ok := auxProviderRetryAfter(err)
	var aws *AWSServiceError
	if !ok && errors.As(err, &aws) {
		value, ok = RetryAfterFromHeaders(aws.Headers)
	}
	if ok && value > 0 {
		return value, true
	}
	return 0, false
}

// Retry-After header of the error response, else a retry_after body field at top level or under error
func auxProviderRetryAfter(err error) (time.Duration, bool) {
	var api *APIError
	if errors.As(err, &api) {
		return ProviderRetryAfter(api)
	}
	var headers http.Header
	var sdk *AuxSDKError
	var cc *ccHTTPError
	var gem *GeminiAPIError
	switch {
	case errors.As(err, &sdk):
		headers = sdk.Headers
	case errors.As(err, &cc):
		headers = cc.Headers
	case errors.As(err, &gem):
		headers = gem.Headers
	}
	if d, ok := RetryAfterFromHeaders(headers); ok && d > 0 {
		return d, true
	}
	if body, ok := auxErrBody(err).(map[string]any); ok {
		target := body
		if nested, isMap := body["error"].(map[string]any); isMap {
			target = nested
		}
		if d, ok := retryAfterValue(target["retry_after"]); ok && d > 0 {
			return d, true
		}
	}
	return 0, false
}

// Wait before same credential retry attempt, false when the declared cooldown exceeds the cap
func auxRateLimitWait(err error, attempt int) (time.Duration, bool) {
	if declared, ok := auxRateLimitRetryAfter(err); ok {
		return declared, declared <= auxRateLimitRetryAfterCap
	}
	return JitteredBackoff(attempt, BackoffOptions{BaseDelay: auxRateLimitBackoffBase, MaxDelay: auxRateLimitBackoffCap, JitterRatio: 0.5}), true
}

// Waits then resends on the same client while the refusal stays a rate limit, returning response, last error, raised error
func auxRateLimitRetryRungs(yield auxLadderYield, firstErr error, client AuxClient, kwargs map[string]any, label string) (any, error, error) {
	err := firstErr
	for attempt := 1; attempt <= auxRateLimitRetries; attempt++ {
		wait, ok := auxRateLimitWait(err, attempt)
		if !ok {
			declared, _ := auxRateLimitRetryAfter(err)
			slog.Info(fmt.Sprintf("Auxiliary %s: provider asked for a %.0fs cooldown; not waiting it out", label, declared.Seconds()))
			break
		}
		slog.Info(fmt.Sprintf("Auxiliary %s: rate limited (attempt %d/%d); retrying after %.2fs", label, attempt, auxRateLimitRetries, wait.Seconds()))
		if _, werr := yield(auxLadderStep{Kind: "wait", Args: []any{wait.Seconds()}}); werr != nil {
			return nil, nil, werr
		}
		resp, accepted, raised := auxRung(yield, auxLadderStep{Kind: "call", Args: []any{client, kwargs}}, auxCredentialRungAccepts)
		if raised != nil {
			return nil, nil, raised
		}
		if accepted == nil {
			return resp, nil, nil
		}
		err = accepted
		if !auxIsRateLimitError(err) || auxIsPaymentError(err) {
			break
		}
	}
	return nil, err, nil
}
