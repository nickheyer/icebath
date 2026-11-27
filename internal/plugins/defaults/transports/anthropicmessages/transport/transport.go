package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Carrier of the replies of this API in a stored message
const carrier = string(provider.ModeAnthropicMessages)

// Requests one call may send again after a refusal it can fix: the 1M beta dropped, refused thinking taken out, fast mode turned off
const recoveries = 3

// Rate limit headers that say a model has no fast mode capacity when they are 0
var fastLimits = []string{"anthropic-fast-input-tokens-limit", "anthropic-fast-output-tokens-limit"}

type registrant struct {
	config    *config.Config
	providers *provider.Registry
	models    *models.Models
	registry  *transport.Registry
	pool      transport.Pool
	claude    claudeCode
	rejected  *rejections
	// What endpoints showed, remembered from then on, by route: the 1M beta refused, tool input to be buffered, streaming refused
	drop1M, buffer, unstreamed sync.Map
	// Models without fast mode capacity, by session
	slowModels sync.Map
}

func Register(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) {
	registry.Register(&registrant{config: cfg, providers: providers, models: m, registry: registry,
		rejected: &rejections{dir: filepath.Join(cfg.Dir(), "cache"), sessions: map[string]map[string]bool{}}})
}

func (r *registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeAnthropicMessages: func(route provider.Route) (provider.Client, error) {
			return &client{r: r, route: route}, nil
		},
	}
}

// Whether the session found the model has no fast mode capacity
func (r *registrant) slow(session, model string) bool {
	_, ok := r.slowModels.Load(session + "\x00" + model)
	return ok
}

type client struct {
	r     *registrant
	route provider.Route
}

func (c *client) Route() provider.Route {
	return c.route
}

// Key of the route, which what its endpoint showed is remembered under
func (c *client) key() string {
	return c.route.Provider + "\x00" + c.route.Model + "\x00" + strings.TrimRight(c.route.BaseURL, "/")
}

func (c *client) buffered(p *prepared) bool {
	_, ok := c.r.buffer.Load(c.key())
	return ok
}

// Error of a request that asked for fast mode on a model with no fast capacity, sent again at standard speed
type slowError struct{ failure *provider.APIError }

func (e *slowError) Error() string { return e.failure.Error() }

func (e *slowError) Unwrap() error { return e.failure }

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	bare := false
	for attempt := 0; ; attempt++ {
		p, err := c.prepare(ctx, request)
		if err != nil {
			return nil, err
		}
		if bare {
			c.unsign(p)
		}
		var response *provider.Response
		delivered := false
		if _, off := c.r.unstreamed.Load(c.key()); off {
			response, err = c.complete(ctx, p, fn)
		} else {
			response, delivered, err = c.stream(ctx, p, fn)
		}
		if err == nil || delivered || ctx.Err() != nil {
			return response, err
		}
		if attempt < recoveries {
			if again, rebuild := c.recover(p, err, fn); again {
				bare = bare || rebuild
				continue
			}
		}
		if _, off := c.r.unstreamed.Load(c.key()); !off && streamless(err, p.where) {
			slog.Info(fmt.Sprintf("Anthropic Messages stream unavailable for %s/%s; switching to non-streaming for this session: %s", c.route.Provider,
				c.route.Model, err))
			c.r.unstreamed.Store(c.key(), true)
			response, err := c.complete(ctx, p, fn)
			if err != nil {
				return nil, err
			}
			return response, nil
		}
		return nil, err
	}
}

// Takes every thinking block out of the prepared messages, for a request whose signatures were refused with none known to blame
func (c *client) unsign(p *prepared) {
	messages, _ := p.body["messages"].([]any)
	converted := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if message, ok := m.(map[string]any); ok {
			converted = append(converted, message)
		}
	}
	strip(converted, nil, true)
	p.thinking = nil
}

// Whether a refused request is sent again, and whether its thinking all goes: the 1M beta a subscription can't use dropped, the refused
// thinking signatures remembered and taken out, fast mode turned off for a model without its capacity
func (c *client) recover(p *prepared, err error, fn provider.StreamFunc) (bool, bool) {
	if slow, ok := errors.AsType[*slowError](err); ok {
		c.r.slowModels.Store(p.request.SessionID+"\x00"+p.model, true)
		transport.Say(fn, provider.EventStatus, fmt.Sprintf("⚠️  Fast mode isn't available for %s on this Anthropic organization — using "+
			"standard speed for this session, retrying...", p.model))
		slog.Warn(fmt.Sprintf("Fast mode: %s has a fast-mode limit of 0; standard speed for this session", p.model), "error", slow.failure)
		return true, false
	}
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok {
		return false, false
	}
	text := strings.ToLower(failure.Message)
	if failure.StatusCode == 400 && strings.Contains(text, "long context beta") && strings.Contains(text, "not yet available") {
		if _, dropped := c.r.drop1M.LoadOrStore(c.key(), true); !dropped {
			slog.Info(fmt.Sprintf("Anthropic rejected the 1M context beta for %s; retrying without it for this client.", p.model))
			return true, false
		}
		return false, false
	}
	if signatureRefused(failure) {
		if len(p.thinking) == 0 {
			slog.Warn("Anthropic refused a thinking signature the request carried none known of; sending it again without its thinking")
			return true, true
		}
		c.r.rejected.add(p.request.SessionID, p.thinking)
		slog.Warn(fmt.Sprintf("Anthropic refused %d replayed thinking signature(s); sending the request again without them for the rest of the "+
			"session", len(p.thinking)))
		return true, false
	}
	return false, false
}

// One streamed attempt: the reply, whether any of it reached the person, and the error that ended it
func (c *client) stream(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, bool, error) {
	address, payload, err := p.target(true)
	if err != nil {
		return nil, false, err
	}
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: timeouts, Model: p.model, Tokens: p.tokens, Emit: fn}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	call, err := c.request(attempt, p, address, payload, true)
	if err != nil {
		return nil, false, err
	}
	stop := watch.Headers(cancel)
	response, err := c.r.pool.Client(p.settings, timeouts.Connect, false).Do(call)
	stop()
	sent := func(err error) error { return &transport.SentError{URL: address, Bytes: len(payload), Err: err} }
	if err != nil {
		return nil, false, p.refused.Transport(ctx, sent(watch.Failed(ctx, err)))
	}
	defer response.Body.Close()
	if attempt.Err() != nil {
		return nil, false, p.refused.Transport(ctx, sent(watch.Failed(ctx, attempt.Err())))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, transport.ErrorBodyLimit))
		return nil, false, c.refusal(p, response, data)
	}
	kind := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(kind, "application/json") && !strings.Contains(kind, "event-stream") && p.where != onBedrock {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, false, p.refused.Transport(ctx, sent(err))
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			return nil, false, p.refused.Stream(transport.FrameError(string(data), response.Header))
		}
		if object, ok := message["error"]; ok && object != nil {
			return nil, false, p.refused.HTTP(response.StatusCode, data, response.Header)
		}
		slog.Info(fmt.Sprintf("Streaming request returned a final message object instead of an event stream; switching %s/%s to non-streaming "+
			"for this session.", c.route.Provider, c.route.Model))
		c.r.unstreamed.Store(c.key(), true)
		reply, err := c.normalize(p, message, response.Header, response.StatusCode)
		if err != nil {
			return nil, false, err
		}
		return reply, false, transport.Replay(reply, fn)
	}
	s := &stream{c: c, p: p, fn: fn, acc: newAccumulator(), header: response.Header.Clone(), status: response.StatusCode}
	next := transport.NewEvents(response.Body).Next
	if p.where == onBedrock {
		next = bedrockEvents(response.Body, response.Header)
	}
	if err := watch.Run(ctx, next, s.frame); err != nil {
		return nil, s.delivered, c.failed(ctx, p, sent, err)
	}
	reply, err := s.finish()
	return reply, s.delivered, err
}

// Error of a stream as its caller reads it: the consumer's own as it was, a provider's error in the stream as its refusal, a failure of
// the connection classified
func (c *client) failed(ctx context.Context, p *prepared, sent func(error) error, err error) error {
	if stop, ok := errors.AsType[*stopped](err); ok {
		return stop.err
	}
	if stream, ok := errors.AsType[*transport.StreamError](err); ok {
		return p.refused.Stream(stream)
	}
	if order, ok := errors.AsType[*orderError](err); ok {
		return p.refused.Empty(order.Error(), order)
	}
	if _, ok := errors.AsType[*provider.APIError](err); ok {
		return err
	}
	return p.refused.Transport(ctx, sent(err))
}

// Error of a refused request, one that asked for fast mode on a model with no fast capacity told apart
func (c *client) refusal(p *prepared, response *http.Response, data []byte) error {
	failure := p.refused.HTTP(response.StatusCode, data, response.Header)
	if response.StatusCode == http.StatusTooManyRequests && p.fast {
		for _, name := range fastLimits {
			if strings.TrimSpace(response.Header.Get(name)) == "0" {
				return &slowError{failure: failure}
			}
		}
	}
	return failure
}

// One request that isn't streamed, waited on for its answer under the silent request limit
func (c *client) complete(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, error) {
	address, payload, err := p.target(false)
	if err != nil {
		return nil, err
	}
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: transport.Timeouts{Stale: p.limits.Call(ctx, p.call.Ask.Requested(), 1)}, Model: p.model, Tokens: p.tokens,
		Emit: fn}
	response, err := transport.Await(ctx, watch, func(ctx context.Context) (*provider.Response, error) {
		return c.once(ctx, p, address, payload, timeouts)
	})
	if err != nil {
		return nil, err
	}
	return response, transport.Replay(response, fn)
}

// The request sent once and its whole answer read, within its time limit
func (c *client) once(ctx context.Context, p *prepared, address string, payload []byte, timeouts transport.Timeouts) (*provider.Response, error) {
	limit := timeouts.Request
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	call, err := c.request(bounded, p, address, payload, false)
	if err != nil {
		return nil, err
	}
	timedOut := func(err error) error {
		if ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return &transport.TimeoutError{Phase: transport.PhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", limit.Seconds())}
		}
		return err
	}
	response, err := c.r.pool.Client(p.settings, timeouts.Connect, false).Do(call)
	if err != nil {
		return nil, p.refused.Transport(ctx, transport.Timed(timedOut(err), transport.PhaseConnect))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, p.refused.Transport(ctx, transport.Timed(timedOut(err), transport.PhaseRead))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, c.refusal(p, response, data)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		failure := p.refused.HTTP(response.StatusCode, data, response.Header)
		failure.Message = "Provider returned a non-JSON message body: " + transport.FrameError(string(data), nil).Text
		failure.Err = err
		return nil, failure
	}
	if object, ok := message["error"]; ok && object != nil && message["type"] == "error" {
		return nil, p.refused.HTTP(response.StatusCode, data, response.Header)
	}
	return c.normalize(p, message, response.Header, response.StatusCode)
}

// HTTP request of the body at the address with the prepared headers, signed when the route signs its requests
func (c *client) request(ctx context.Context, p *prepared, address string, payload []byte, stream bool) (*http.Request, error) {
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	call.Header = p.header.Clone()
	if stream && p.where == direct {
		call.Header.Set("Accept", "text/event-stream")
	}
	if p.where == onBedrock && stream {
		call.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
	if c.route.Sign != nil {
		if err := c.route.Sign(call, payload); err != nil {
			return nil, fmt.Errorf("The request to %s couldn't be signed: %w", c.route.Provider, err)
		}
	}
	return call, nil
}
