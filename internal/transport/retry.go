package transport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
	"github.com/nickheyer/icebath/pkg/strs"
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
				Message: fmt.Sprintf("The provider sent nothing back %s in a row, so icebath stopped asking. Switch models or start a new "+
					"session, then try again.", strs.Count(streak, "time")), Own: true}
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
		Say(fn, provider.EventStatus, "The connection dropped while the model was writing a tool call. Connecting again.")
	}
	route := r.client.Route()
	name := route.Provider
	name = cmp.Or(name, "provider")
	reason := dropReason(err)
	slog.Warn(fmt.Sprintf("Stream %s on attempt %d/%d — retrying. provider=%s base_url=%s error=%s", kind, attempt, attempts, name, route.BaseURL,
		strs.Head(err.Error(), 240)), "mid_tool_call", tool)
	Say(fn, provider.EventStatus, fmt.Sprintf("The reply from %s dropped after %s: %s. Connecting again, try %d of %d.", name,
		strs.Span(max(after.Round(100*time.Millisecond), 100*time.Millisecond)), reason, attempt, attempts))
	Say(fn, provider.EventActivity, fmt.Sprintf("connecting again, try %d of %d, after %s", attempt, attempts, reason))
}

// Short reason of a dropped stream
func dropReason(err error) string {
	switch {
	case Phase(err) != "":
		return "it took too long"
	case unreached(err):
		return "the connection was lost"
	}
	return strs.Head(strings.Join(strings.Fields(err.Error()), " "), 80)
}

// Line once every retry of a stream failed: an empty stream, the lifetime cut, or the endpoint that couldn't be reached and why
func (r *retried) exhausted(fn provider.StreamFunc, failure *provider.APIError, err error, attempts int) {
	tries := strs.Count(attempts, "try")
	if failure.Kind == provider.ErrEmptyResponse {
		Say(fn, provider.EventStatus, fmt.Sprintf("⚠ The provider sent back empty replies %s, so it may be having trouble. Try again in a "+
			"moment.", map[bool]string{true: "once", false: strs.Count(attempts, "time")}[attempts == 1]))
		return
	}
	if cutoff, ok := errors.AsType[*LifetimeError](err); ok {
		Say(fn, provider.EventStatus, fmt.Sprintf("⚠ %s That happened on each of %s.", cutoff.Error(), tries))
		return
	}
	raw, size := r.client.Route().BaseURL, -1
	if sent, ok := errors.AsType[*SentError](err); ok {
		raw, size = cmp.Or(sent.URL, raw), sent.Bytes
	}
	host := "the provider"
	if u, perr := url.Parse(raw); perr == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	line := fmt.Sprintf("⚠ icebath could not get a reply from %s in %s", host, tries)
	switch {
	case size < 0:
		line += ", and it looks unreachable. Try again in a moment."
	case size >= largeRequest:
		line += fmt.Sprintf(". The request was %s, and the provider or a proxy in front of it may refuse requests that large.", strs.Size(float64(size)))
	default:
		line += fmt.Sprintf(", and it looks unreachable with a request of %s. Try again in a moment.", strs.Size(float64(size)))
	}
	Say(fn, provider.EventStatus, line)
}
