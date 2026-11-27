package transport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/strs"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

// Request size from which an endpoint that can't be reached may be refusing its size
const largeRequest = 256 * 1024

// Request a wire sent, which an error of it carries for the notices of its failure: where it went and how large it was
type SentError struct {
	URL   string
	Bytes int
	Err   error
}

func (e *SentError) Error() string { return e.Err.Error() }

func (e *SentError) Unwrap() error { return e.Err }

// Stalled streams in a row of each session, which past the setting fail its requests at once
type stalls struct {
	mu       sync.Mutex
	sessions map[string]int
}

func (s *stalls) count(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[session]
}

func (s *stalls) bump(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session]++
	return s.sessions[session]
}

func (s *stalls) clear(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, session)
}

// Client whose streams that broke before they delivered anything are asked again, as often as transports.stream_retries allows, and
// whose session's requests fail at once after too many stalls in a row
type retried struct {
	client provider.Client
	config *config.Config
	stalls *stalls
}

func (r *retried) Route() provider.Route {
	return r.client.Route()
}

func (r *retried) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	cfg := r.config
	if request.Config != nil {
		cfg = request.Config
	}
	retries := max(cfg.GetInt(transportconfig.StreamRetries), 0)
	giveup := cfg.GetInt(transportconfig.StaleGiveup)
	// Stalls count for a session on one route, a fallback or another model starting afresh; requests of no session aren't counted
	route := r.client.Route()
	session := ""
	if request.SessionID != "" {
		session = request.SessionID + "\x00" + route.Provider + "\x00" + request.Model + "\x00" + route.BaseURL
	}
	for attempt := 0; ; attempt++ {
		if streak := r.stalls.count(session); session != "" && giveup > 0 && streak >= giveup {
			return nil, &provider.APIError{Provider: r.client.Route().Provider, Model: request.Model, Kind: provider.ErrNetwork,
				Message: fmt.Sprintf("Provider has been unresponsive (no response received) for %d consecutive stale attempts — aborting this call to "+
					"avoid an indefinite stall. Switch models or start a new session, then retry.", streak)}
		}
		delivered, tool := false, false
		started := time.Now()
		response, err := r.client.Stream(ctx, request, func(event provider.StreamEvent) error {
			delivered = delivered || event.Delivers()
			tool = tool || event.Kind == provider.EventToolStart
			if fn == nil {
				return nil
			}
			return fn(event)
		})
		if err == nil {
			r.stalls.clear(session)
			return response, nil
		}
		if _, stalled := errors.AsType[*StallError](err); stalled && session != "" {
			r.stalls.bump(session)
		} else if waited, ok := errors.AsType[*WaitedError](err); ok && waited.Waited > 0 && session != "" {
			slog.Warn(fmt.Sprintf("Interrupted provider wait counted as stale; consecutive stale attempts=%d.", r.stalls.bump(session)))
		}
		if ctx.Err() != nil {
			return nil, err
		}
		failure, ok := errors.AsType[*provider.APIError](err)
		transient := ok && failure.StatusCode == 0 && (failure.Kind == provider.ErrNetwork || failure.Kind == provider.ErrEmptyResponse)
		if delivered {
			if transient {
				slog.Warn("Streaming failed after partial delivery, not retrying", "error", err)
			}
			return nil, err
		}
		if !transient {
			return nil, err
		}
		if attempt >= retries {
			r.exhausted(fn, failure, err, retries+1)
			return nil, err
		}
		r.dropped(fn, err, attempt+1, retries+1, tool, time.Since(started))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(4*time.Second)))):
		}
	}
}

// Lines of a stream dropped before it delivered anything, which is asked again
func (r *retried) dropped(fn provider.StreamFunc, err error, attempt, attempts int, tool bool, after time.Duration) {
	kind := "drop"
	if tool {
		kind = "drop mid tool-call"
		Say(fn, provider.EventStatus, "⚠ Connection dropped mid tool-call; reconnecting…")
	}
	route := r.client.Route()
	name := route.Provider
	name = cmp.Or(name, "provider")
	reason := dropReason(err)
	slog.Warn(fmt.Sprintf("Stream %s on attempt %d/%d — retrying. provider=%s base_url=%s error=%s", kind, attempt, attempts, name, route.BaseURL,
		strs.Head(err.Error(), 240)), "mid_tool_call", tool)
	Say(fn, provider.EventStatus, fmt.Sprintf("⚠️ %s stream %s (%s) after %.1fs — attempt %d/%d dropped, reconnecting", name, kind, reason,
		after.Seconds(), attempt, attempts))
	Say(fn, provider.EventActivity, fmt.Sprintf("stream retry %d/%d after %s", attempt, attempts, reason))
}

// Short reason of a dropped stream
func dropReason(err error) string {
	switch {
	case Phase(err) != "":
		return "timeout"
	case unreached(err):
		return "connection lost"
	}
	return strs.Head(strings.Join(strings.Fields(err.Error()), " "), 80)
}

// Line once every retry of a stream failed: an empty stream, the lifetime cut, or the endpoint that couldn't be reached and why
func (r *retried) exhausted(fn provider.StreamFunc, failure *provider.APIError, err error, attempts int) {
	plural := "s"
	if attempts == 1 {
		plural = ""
	}
	if failure.Kind == provider.ErrEmptyResponse {
		Say(fn, provider.EventStatus, fmt.Sprintf("❌ Provider returned an empty response stream after %d attempts. The provider may be experiencing "+
			"issues — try again in a moment.", attempts))
		return
	}
	if cutoff, ok := errors.AsType[*LifetimeError](err); ok {
		Say(fn, provider.EventStatus, fmt.Sprintf("❌ %s (%d attempt%s)", cutoff.Error(), attempts, plural))
		return
	}
	raw, size := r.client.Route().BaseURL, -1
	if sent, ok := errors.AsType[*SentError](err); ok {
		raw, size = cmp.Or(sent.URL, raw), sent.Bytes
	}
	host := "the endpoint"
	if u, perr := url.Parse(raw); perr == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	line := fmt.Sprintf("❌ Could not open a stream to %s after %d attempt%s", host, attempts, plural)
	switch {
	case size < 0:
		line += "; the endpoint looks unreachable — try again in a moment."
	case size >= largeRequest:
		line += fmt.Sprintf(" (request %d KB); the endpoint or a proxy in front of it may reject requests this large.", max(1, int(math.RoundToEven(float64(size)/1024))))
	default:
		line += fmt.Sprintf(" (request %d KB); the endpoint looks unreachable — try again in a moment.", max(1, int(math.RoundToEven(float64(size)/1024))))
	}
	Say(fn, provider.EventStatus, line)
}
