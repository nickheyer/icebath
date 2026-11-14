package model

import (
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	RetryAfterCap    = 600 * time.Second
	LiveRetryWaitCap = 60 * time.Second
)

const (
	PolicyZaiShort = "zai_coding_overload_short"
	PolicyZaiLong  = "zai_coding_overload_long"
)

const zaiShortAttempts = 3

var zaiLongBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 120 * time.Second}

func Backoff(attempt int) time.Duration {
	return httpclient.Backoff(attempt, 5*time.Second, 120*time.Second, 0.5)
}

func ProviderRetryAfter(err error) (time.Duration, bool) {
	var ae *wire.APIError
	if !errors.As(err, &ae) {
		return 0, false
	}
	now := time.Now()
	d, ok := ae.RetryAfter(now)
	if !ok {
		body, _ := wire.ParseValue(ae.Body())
		holder := body
		if nested := body.Get("error"); nested.IsObject() {
			holder = nested
		}
		var seconds float64
		if seconds, ok = retryAfterValue(holder.Get("retry_after"), now); ok {
			d = secondsDuration(seconds)
		}
	}
	if !ok || d <= 0 {
		return 0, false
	}
	return d, true
}

func IsZaiCodingOverload(err error, baseURL, model string) bool {
	var ae *wire.APIError
	if !errors.As(err, &ae) || ae.Status != 429 {
		return false
	}
	if !strings.Contains(strings.ToLower(baseURL), "api.z.ai/api/coding/paas/v4") ||
		!strings.Contains(strings.ToLower(model), "glm-5.2") {
		return false
	}
	text := strings.ToLower(err.Error() + " " + ae.Message() + " " + string(ae.Body()))
	return strings.Contains(text, "1305") || strings.Contains(text, "temporarily overloaded")
}

func ZaiOverloadBackoff(attempt int, err error, baseURL, model string, defaultWait time.Duration) (time.Duration, string) {
	if !IsZaiCodingOverload(err, baseURL, model) {
		return defaultWait, ""
	}
	if attempt <= zaiShortAttempts {
		return defaultWait, PolicyZaiShort
	}
	base := zaiLongBackoff[min(attempt-zaiShortAttempts-1, len(zaiLongBackoff)-1)]
	return httpclient.Backoff(1, base, base, 0.2), PolicyZaiLong
}

func ZaiOverloadRetryCeiling() int {
	return zaiShortAttempts + len(zaiLongBackoff) + 1
}

type Wait struct {
	Delay      time.Duration
	RetryAfter bool
	Adaptive   bool
	Policy     string
	Announce   bool
}

func RetryWait(attempt int, e *Error, baseURL string) Wait {
	rateLimited := e.Class == ClassRateLimit || e.Class == ClassBilling || e.Class == ClassUpstreamRateLimit
	retryAfter, declared := ProviderRetryAfter(e.Err)
	w := Wait{RetryAfter: declared, Adaptive: rateLimited || IsZaiCodingOverload(e.Err, baseURL, e.Model)}
	if declared {
		w.Delay = min(retryAfter, RetryAfterCap)
	} else {
		w.Delay = httpclient.Backoff(attempt, 2*time.Second, 60*time.Second, 0.5)
	}
	if w.Adaptive && !declared {
		w.Delay, w.Policy = ZaiOverloadBackoff(attempt, e.Err, baseURL, e.Model, w.Delay)
	}
	if w.Adaptive {
		w.Announce = w.Policy == PolicyZaiLong
	} else {
		w.Announce = declared && w.Delay > LiveRetryWaitCap
	}
	return w
}
