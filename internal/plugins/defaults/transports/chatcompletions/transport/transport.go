package transport

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Carrier of the replies of this API in a stored message
const carrier = string(provider.ModeChatCompletions)

// Window in which a stream a server error broke is asked again without streaming once per session
const unmaskWindow = time.Minute

type registrant struct {
	config    *config.Config
	providers *provider.Registry
	models    *models.Models
	// What endpoints showed they can't take, remembered from then on: stream_options by base URL, streaming by route, and when a session last
	// had a streamed server error asked again without streaming
	refused    *sync.Map
	unstreamed *sync.Map
	unmasked   *sync.Map
	pool       *transport.Pool
	registry   *transport.Registry
}

func newRegistrant(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) *registrant {
	return &registrant{config: cfg, providers: providers, models: m, refused: &sync.Map{}, unstreamed: &sync.Map{}, unmasked: &sync.Map{},
		pool: &transport.Pool{}, registry: registry}
}

func Register(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) {
	registry.Register(newRegistrant(registry, cfg, providers, m))
}

func (r *registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeChatCompletions: func(route provider.Route) (provider.Client, error) {
			if strings.TrimSpace(route.BaseURL) == "" {
				return nil, fmt.Errorf("%s has no base URL for %s.", route.Provider, route.Model)
			}
			return &client{r: r, route: route}, nil
		},
	}
}

type client struct {
	r     *registrant
	route provider.Route
}

func (c *client) Route() provider.Route {
	return c.route
}

// Key of the route whose streaming a server turned down
func (c *client) key() string {
	return c.route.Provider + "\x00" + c.route.Model + "\x00" + strings.TrimRight(c.route.BaseURL, "/")
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	p, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	if _, off := c.r.unstreamed.Load(c.key()); off {
		return c.complete(ctx, p, fn)
	}
	for attempt := 0; ; attempt++ {
		response, delivered, err := c.stream(ctx, p, fn)
		if err == nil || delivered || ctx.Err() != nil {
			return response, err
		}
		failure, _ := errors.AsType[*provider.APIError](err)
		if _, already := c.r.refused.Load(c.base()); attempt == 0 && !already && failure != nil && rejectsStreamOptions(failure) {
			c.r.refused.Store(c.base(), true)
			slog.Info(fmt.Sprintf("Endpoint rejected stream_options (HTTP %d); retrying without it for this client.", failure.StatusCode))
			continue
		}
		if !p.llamacpp {
			if stream, ok := errors.AsType[*transport.StreamError](err); ok && streamCode(stream) == transport.CodeEmptyFrame {
				c.r.unstreamed.Store(c.key(), true)
				slog.Warn(fmt.Sprintf("Provider stream returned an empty SSE frame (keepalive, no payload) before any delta — switching %s/%s to "+
					"non-streaming for this session.", cmp.Or(c.route.Provider, "unknown"), cmp.Or(c.route.Model, "unknown")))
				transport.Say(fn, provider.EventStatus, "⚠️ Provider stream returned an empty keepalive frame — retrying this turn without streaming (streaming stays "+
					"off for this session).")
				return c.complete(ctx, p, fn)
			}
			if failure != nil && unsupportedStreaming(failure) {
				c.r.unstreamed.Store(c.key(), true)
				transport.Say(fn, provider.EventStatus, "⚠  Streaming is not supported for this model/provider. Switching to non-streaming.")
				return c.complete(ctx, p, fn)
			}
			if response, done, err := c.unmask(ctx, p, fn, failure, err); done {
				return response, err
			}
		}
		return nil, err
	}
}

// One request without streaming after a server error broke the stream before it delivered anything, once a minute per session: its
// answer when it gives one, its own refusal when that names the cause; done false leaves the stream's error standing
func (c *client) unmask(ctx context.Context, p *prepared, fn provider.StreamFunc, failure *provider.APIError, cause error) (*provider.Response,
	bool, error) {
	if failure == nil || failure.StatusCode < 500 {
		return nil, false, cause
	}
	key := p.request.SessionID + "\x00" + c.key()
	if at, ok := c.r.unmasked.Load(key); ok && time.Since(at.(time.Time)) < unmaskWindow {
		return nil, false, cause
	}
	c.r.unmasked.Store(key, time.Now())
	response, err := c.complete(ctx, p, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true, err
		}
		if probe, ok := errors.AsType[*provider.APIError](err); ok && probe.StatusCode > 0 && probe.StatusCode < 500 {
			slog.Info("Non-streaming unmask probe surfaced the underlying error", "error", err)
			return nil, true, err
		}
		slog.Info("Non-streaming unmask probe failed", "error", err)
		return nil, false, cause
	}
	slog.Info(fmt.Sprintf("Streaming 5xx re-issued non-streaming successfully for %s/%s (not latched: the 5xx may be transient).",
		cmp.Or(c.route.Provider, "unknown"), cmp.Or(c.route.Model, "unknown")))
	transport.Say(fn, provider.EventStatus, "⚠  Streaming failed with a provider server error; the non-streaming retry succeeded.")
	return response, true, transport.Replay(response, fn)
}

func (c *client) base() string {
	return strings.TrimRight(c.route.BaseURL, "/")
}

// Code of the error object a stream error carries
func streamCode(err *transport.StreamError) string {
	object, _ := err.Body["error"].(map[string]any)
	code, _ := object["code"].(string)
	return code
}

// Whether an endpoint refused the request for its stream_options, as some take it for an unknown field
func rejectsStreamOptions(failure *provider.APIError) bool {
	if failure.StatusCode != http.StatusBadRequest && failure.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	text := strings.ToLower(failure.Message + " " + errorText(failure.Err))
	return strings.Contains(text, "stream_options") && provider.Contains(text, "extra", "not supported", "unrecognized", "unexpected", "unknown",
		"not permitted", "additional propert")
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Whether the provider said it streams nothing for the model
func unsupportedStreaming(failure *provider.APIError) bool {
	text := strings.ToLower(failure.Message)
	return strings.Contains(text, "stream") && strings.Contains(text, "not supported")
}

// One streamed attempt: the reply, whether any of it reached the person, and the error that ended it
func (c *client) stream(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, bool, error) {
	body := maps.Clone(p.body)
	body["stream"] = true
	if _, refused := c.r.refused.Load(c.base()); !refused && !p.nativeGemini {
		body["stream_options"] = map[string]any{"include_usage": true}
	} else {
		delete(body, "stream_options")
	}
	if _, set := body["return_progress"]; p.llamacpp && !set {
		body["return_progress"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("The chat completions request couldn't be encoded: %w", err)
	}
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: timeouts, Model: p.request.Model, Tokens: p.tokens, Emit: fn}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	call, err := c.request(attempt, p, payload)
	if err != nil {
		return nil, false, err
	}
	stop := watch.Headers(cancel)
	response, err := c.r.pool.Client(p.settings, timeouts.Connect, true).Do(call)
	stop()
	sent := func(err error) error { return &transport.SentError{URL: p.url, Bytes: len(payload), Err: err} }
	if err != nil {
		return nil, false, p.refused.Transport(ctx, sent(watch.Failed(ctx, err)))
	}
	defer response.Body.Close()
	if attempt.Err() != nil {
		return nil, false, p.refused.Transport(ctx, sent(watch.Failed(ctx, attempt.Err())))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, transport.ErrorBodyLimit))
		return nil, false, p.refused.HTTP(response.StatusCode, data, response.Header)
	}
	reader, err := decoded(response)
	if err != nil {
		return nil, false, p.refused.Transport(ctx, sent(err))
	}
	kind := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(kind, "application/json") && !strings.Contains(kind, "event-stream") {
		return c.final(ctx, p, fn, reader, response)
	}
	s := newStream(c, p, fn, response)
	events := transport.NewEvents(reader)
	if err := watch.Run(ctx, events.Next, s.frame); err != nil {
		return nil, s.delivered, failed(ctx, p, sent, err)
	}
	reply, err := s.finish()
	if err != nil {
		return nil, s.delivered, failed(ctx, p, sent, err)
	}
	return reply, s.delivered, nil
}

// Error of a stream as its caller reads it: the consumer's own error as it was, a provider's error in the stream as its refusal, a failure of
// the connection classified
func failed(ctx context.Context, p *prepared, sent func(error) error, err error) error {
	if stop, ok := errors.AsType[*stopped](err); ok {
		return stop.err
	}
	if stream, ok := errors.AsType[*transport.StreamError](err); ok {
		return p.refused.Stream(stream)
	}
	if _, ok := errors.AsType[*provider.APIError](err); ok {
		return err
	}
	return p.refused.Transport(ctx, sent(err))
}

// Answer to a streamed request that came back whole: its error, else the reply, with streaming off for the route from then on
func (c *client) final(ctx context.Context, p *prepared, fn provider.StreamFunc, reader io.Reader, response *http.Response) (*provider.Response,
	bool, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, false, p.refused.Transport(ctx, err)
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil, false, p.refused.Stream(transport.FrameError(string(data), response.Header))
	}
	if object, ok := payload["error"]; ok && object != nil {
		if inner, ok := object.(map[string]any); !ok || len(inner) > 0 {
			return nil, false, p.refused.HTTP(response.StatusCode, data, response.Header)
		}
	}
	slog.Info(fmt.Sprintf("Streaming request returned a final response object instead of an iterator; switching %s/%s to non-streaming for this "+
		"session.", cmp.Or(c.route.Provider, "unknown"), cmp.Or(c.route.Model, "unknown")))
	c.r.unstreamed.Store(c.key(), true)
	reply, err := c.normalize(p, payload, response.Header)
	if err != nil {
		return nil, false, err
	}
	return reply, false, transport.Replay(reply, fn)
}

// Body of an answer, inflated when the server sent it gzipped on its own
func decoded(response *http.Response) (io.Reader, error) {
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		return gzip.NewReader(response.Body)
	}
	return response.Body, nil
}

// One request that isn't streamed, waited on for its answer under the silent request limit
func (c *client) complete(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, error) {
	body := maps.Clone(p.body)
	delete(body, "stream")
	delete(body, "stream_options")
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("The chat completions request couldn't be encoded: %w", err)
	}
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: transport.Timeouts{Stale: p.limits.Call(ctx, p.call.Ask.Requested(), 1)}, Model: p.request.Model,
		Tokens: p.tokens, Emit: fn}
	response, err := transport.Await(ctx, watch, func(ctx context.Context) (*provider.Response, error) {
		return c.once(ctx, p, payload, timeouts)
	})
	if err != nil {
		return nil, err
	}
	return response, transport.Replay(response, fn)
}

// The request sent once and its whole answer read, within its time limit
func (c *client) once(ctx context.Context, p *prepared, payload []byte, timeouts transport.Timeouts) (*provider.Response, error) {
	limit := timeouts.Request
	connect := 30 * time.Second
	if timeouts.Configured {
		connect = min(limit, time.Minute)
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	call, err := c.request(bounded, p, payload)
	if err != nil {
		return nil, err
	}
	timedOut := func(err error) error {
		if ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return &transport.TimeoutError{Phase: transport.PhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", limit.Seconds())}
		}
		return err
	}
	response, err := c.r.pool.Client(p.settings, connect, true).Do(call)
	if err != nil {
		return nil, p.refused.Transport(ctx, transport.Timed(timedOut(err), transport.PhaseConnect))
	}
	defer response.Body.Close()
	reader, err := decoded(response)
	if err != nil {
		return nil, p.refused.Transport(ctx, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, p.refused.Transport(ctx, transport.Timed(timedOut(err), transport.PhaseRead))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, p.refused.HTTP(response.StatusCode, data, response.Header)
	}
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		failure := p.refused.HTTP(response.StatusCode, data, response.Header)
		failure.Message = "Provider returned a non-JSON completion body: " + strs.Head(strings.TrimSpace(transport.FrameError(string(data), nil).Text), 300)
		failure.Err = err
		return nil, failure
	}
	return c.normalize(p, reply, response.Header)
}

// HTTP request of the prepared body with its headers
func (c *client) request(ctx context.Context, p *prepared, payload []byte) (*http.Request, error) {
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	call.Header = p.header.Clone()
	if c.route.Sign != nil {
		if err := c.route.Sign(call, payload); err != nil {
			return nil, fmt.Errorf("The request to %s couldn't be signed: %w", c.route.Provider, err)
		}
	}
	return call, nil
}
