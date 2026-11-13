package httpclient

import (
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Repeats a request with jittered exponential backoff, honouring the server wait header and the request context
type Retry struct {
	Attempts  int
	Base      time.Duration
	Max       time.Duration
	Jitter    float64
	MaxWait   time.Duration
	Retryable func(req *http.Request, resp *http.Response, err error) bool
}

// Sends the request until it succeeds, stops being retryable, or uses every attempt, a body it cannot replay sent once
func (r Retry) Do(c *http.Client, req *http.Request) (*http.Response, error) {
	attempts := max(1, r.Attempts)
	if attempts > 1 {
		replayable, done := replayBody(req)
		defer done()
		if !replayable {
			attempts = 1
		}
	}
	retryable := r.Retryable
	if retryable == nil {
		retryable = defaultRetryable
	}
	ctx := req.Context()
	for attempt := 1; ; attempt++ {
		try := req
		if attempt > 1 {
			try = req.Clone(ctx)
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				try.Body = body
			}
		}
		resp, err := c.Do(try)
		if attempt >= attempts || ctx.Err() != nil || !retryable(try, resp, err) {
			return resp, err
		}
		wait := r.wait(attempt, resp)
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

// Pause before the next attempt, a positive Retry-After capped by MaxWait winning over the backoff
func (r Retry) wait(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if d, ok := RetryAfter(resp.Header, time.Now()); ok && d > 0 {
			if r.MaxWait > 0 && d > r.MaxWait {
				return r.MaxWait
			}
			return d
		}
	}
	return Backoff(attempt, r.Base, r.Max, r.Jitter)
}

// Body readable from offset zero by every attempt
type replaySource interface {
	io.ReaderAt
	io.Seeker
	io.Closer
}

// Gives a positioned file body an independent view per attempt, reporting false when only one send is possible
func replayBody(req *http.Request) (bool, func()) {
	if req.Body == nil || req.Body == http.NoBody || req.GetBody != nil {
		return true, func() {}
	}
	source, ok := req.Body.(replaySource)
	if !ok {
		return false, func() {}
	}
	start, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, func() {}
	}
	end, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return false, func() {}
	}
	if end < start {
		source.Seek(start, io.SeekStart)
		return false, func() {}
	}
	view := func() (io.ReadCloser, error) { return io.NopCloser(io.NewSectionReader(source, start, end-start)), nil }
	req.Body, _ = view()
	req.GetBody = view
	return true, func() { source.Close() }
}

// Retries failures that never reached the server, and others only when repeating the method is safe
func defaultRetryable(req *http.Request, resp *http.Response, err error) bool {
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			return false
		}
		switch e.Phase {
		case PhasePool, PhaseConnect:
			return true
		case PhaseProxy:
			var pe *ProxyError
			return !errors.As(err, &pe) || pe.Status == 0 || pe.Status >= 500
		case PhaseTLS:
			return e.Timeout()
		case PhaseWrite, PhaseHeader, PhaseBody:
			return idempotent(req)
		}
		return false
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return idempotent(req)
	}
	return false
}

// Reports whether sending the request twice has the effect of sending it once
func idempotent(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != ""
}

// Exponential delay doubling from base up to ceiling, plus a uniform share of up to jitter times the delay
func Backoff(attempt int, base, ceiling time.Duration, jitter float64) time.Duration {
	exponent := max(0, attempt-1)
	delay := ceiling
	if base > 0 && exponent < 63 {
		scaled := float64(base) * math.Pow(2, float64(exponent))
		if ceiling <= 0 || scaled < float64(ceiling) {
			delay = clampDuration(scaled)
		}
	}
	if delay < 0 {
		delay = 0
	}
	if jitter > 0 {
		delay = clampDuration(float64(delay) + rand.Float64()*jitter*float64(delay))
	}
	return delay
}

// Duration of a nanosecond count, saturating at the largest duration
func clampDuration(ns float64) time.Duration {
	if ns >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}

// Wait a Retry-After header asks for, false when absent or unreadable
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	return ParseRetryAfter(h.Get("Retry-After"), now)
}

// Wait of a Retry-After value in delta seconds, fractional seconds or an HTTP or RFC 5322 date, never negative
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if math.IsNaN(f) || f <= 0 {
			return 0, true
		}
		return clampDuration(f * float64(time.Second)), true
	}
	when, err := http.ParseTime(text)
	if err != nil {
		if when, err = mail.ParseDate(text); err != nil {
			return 0, false
		}
	}
	return max(0, when.Sub(now)), true
}
