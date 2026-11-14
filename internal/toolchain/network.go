package toolchain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

// Retry budget of one idempotent request and the ceilings on a single wait
const (
	retryAttempts    = 4
	maxRetryDelay    = 30 * time.Second
	maxRecoveryDelay = 300 * time.Second
)

// Body that ended before the bytes its headers promised
var errIncomplete = errors.New("response ended before the complete body arrived")

// Statuses worth another attempt against the same host
var retryStatuses = map[int]bool{
	http.StatusRequestTimeout: true, http.StatusTooManyRequests: true, http.StatusInternalServerError: true,
	http.StatusBadGateway: true, http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true,
}

// Reports whether another attempt at the same request can succeed
func isTransient(err error) bool {
	var st *statusError
	if errors.As(err, &st) {
		return retryStatuses[st.code]
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, errIncomplete) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTemporary || dns.IsTimeout
	}
	var he *httpclient.Error
	if errors.As(err, &he) {
		if he.Timeout() {
			return true
		}
		if he.Phase == httpclient.PhaseTLS {
			return errors.Is(err, io.EOF)
		}
	}
	for _, errno := range connectionErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Wait before attempt, doubling from one second and never shorter than the server asked
func retryDelay(err error, attempt int) time.Duration {
	delay := time.Second << min(attempt, 20)
	var st *statusError
	if errors.As(err, &st) {
		if asked, ok := httpclient.RetryAfter(st.header, time.Now()); ok && asked > delay {
			delay = asked
		}
	}
	return delay
}

// Repeats op while it fails transiently, durable progress renewing the budget and a mirror cutting long waits short
func retryNetwork(ctx context.Context, op func() error, progress func() int64, fallbackAvailable bool) error {
	failures, backoff := 0, 0
	var saved int64
	if progress != nil {
		saved = progress()
	}
	for {
		err := op()
		if err == nil || ctx.Err() != nil || !isTransient(err) {
			return err
		}
		current := saved
		if progress != nil {
			current = progress()
		}
		if current > saved {
			failures, backoff, saved = 0, 0, current
		} else {
			failures++
		}
		if failures >= retryAttempts {
			return err
		}
		delay := retryDelay(err, backoff)
		ceiling := maxRecoveryDelay
		if fallbackAvailable {
			ceiling = maxRetryDelay
		}
		switch {
		case progress == nil:
			delay = min(delay, maxRetryDelay)
		case delay > ceiling:
			return err
		}
		backoff++
		slog.Warn(fmt.Sprintf("network request failed (%v); retrying in %gs (%d/%d failures without progress)",
			err, delay.Seconds(), failures, retryAttempts))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
