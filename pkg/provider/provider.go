// Binds a wire protocol to an endpoint, a credential and an HTTP retry policy
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Attaches a credential to an outgoing request
type Auth interface {
	Apply(r *http.Request)
}

// Credential sent as Authorization Bearer
type Bearer string

func (b Bearer) Apply(r *http.Request) { r.Header.Set("Authorization", "Bearer "+string(b)) }

// Credential sent in a named header such as x-api-key
type Header struct {
	Name  string
	Value string
}

func (h Header) Apply(r *http.Request) { r.Header.Set(h.Name, h.Value) }

// Credential sent as a named query parameter
type Query struct {
	Name  string
	Value string
}

func (q Query) Apply(r *http.Request) {
	values := r.URL.Query()
	values.Set(q.Name, q.Value)
	r.URL.RawQuery = values.Encode()
}

// Auth that must do work before it can sign, such as loading or refreshing a credential
type Refresher interface {
	Refresh(ctx context.Context) error
}

// Credential for endpoints that need none
type None struct{}

func (None) Apply(*http.Request) {}

// Attempts one call gets and the exponential backoff bounds between them
const (
	retryAttempts = 3
	retryBase     = 500 * time.Millisecond
	retryMax      = 8 * time.Second
)

// Bounds on a call the endpoint can otherwise hold open forever, connection setup, the wait for the response headers
// and the longest silence a body may fall into between bytes
const (
	DialTimeout   = 10 * time.Second
	HeaderTimeout = 10 * time.Minute
	StallTimeout  = 10 * time.Minute
)

// Client every provider sends through, its transport bounding connection setup and keeping idle connections warm
var client = &http.Client{Transport: &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout: DialTimeout,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        16,
	IdleConnTimeout:     90 * time.Second,
}}

type labelKey struct{}

// Carries a label, such as a run id, that every attempt made under the context reports
func WithLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, labelKey{}, label)
}

// Label carried by the context, empty when none was set
func LabelFrom(ctx context.Context) string {
	label, _ := ctx.Value(labelKey{}).(string)
	return label
}

// Error of a call the endpoint left silent for longer than the bound
type StallError struct {
	Provider string
	// Whether the silence fell before the headers or inside the body
	Headers bool
	After   time.Duration
}

func (e *StallError) Error() string {
	if e.Headers {
		return fmt.Sprintf("provider %s: no response headers after %s", e.Provider, e.After)
	}
	return fmt.Sprintf("provider %s: no data for %s, the stream stalled", e.Provider, e.After)
}

// Headers and body fields sent with a turn that asks for one directive
type Directive struct {
	Header http.Header                `json:"header,omitempty"`
	Extra  map[string]json.RawMessage `json:"extra,omitempty"`
}

// Deep copy sharing nothing with the original
func (d Directive) Clone() Directive {
	d.Header = d.Header.Clone()
	if d.Extra != nil {
		extra := make(map[string]json.RawMessage, len(d.Extra))
		for k, v := range d.Extra {
			extra[k] = append(json.RawMessage(nil), v...)
		}
		d.Extra = extra
	}
	return d
}

// Protocol plus everything the protocol leaves to the endpoint
type Provider struct {
	Name     string
	Protocol protocol.Protocol
	BaseURL  string
	Auth     Auth
	// Static headers every request carries after the protocol headers
	Header http.Header
	// Static query parameters every request carries
	Query url.Values
	// Request fields merged into every turn beneath the fields the request itself carries
	Extra map[string]json.RawMessage
	// Headers and body fields laid over every turn, how the endpoint hears what the profile asks of it
	Directives []Directive
	// Receives every attempt with its status, headers and backoff
	Observer Observer
	// Bounds on the wait for the headers and on silence inside the body, the package defaults when zero
	HeaderTimeout time.Duration
	StallTimeout  time.Duration
}

// Bound on the wait for the response headers
func (p *Provider) headerTimeout() time.Duration {
	if p.HeaderTimeout > 0 {
		return p.HeaderTimeout
	}
	return HeaderTimeout
}

// Bound on the silence between bytes of a body
func (p *Provider) stallTimeout() time.Duration {
	if p.StallTimeout > 0 {
		return p.StallTimeout
	}
	return StallTimeout
}

// Sends one turn without streaming and decodes the complete response
func (p *Provider) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	req.Stream = false
	resp, err := p.do(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	defer resp.Body.Close()
	return p.Protocol.Decode(resp.Body)
}

// Sends one turn and yields the protocol events of its streamed response
func (p *Provider) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		req.Stream = true
		resp, err := p.do(ctx, req)
		if err != nil {
			yield(protocol.Event{}, err)
			return
		}
		defer resp.Body.Close()
		for ev, err := range p.Protocol.Stream(resp.Body) {
			if !yield(ev, err) || err != nil {
				return
			}
		}
	}
}

// Encodes the request with provider extras beneath it and directives over it, then issues it
func (p *Provider) do(ctx context.Context, req protocol.Request) (*http.Response, error) {
	if p.Protocol == nil {
		return nil, errors.New("provider: no protocol")
	}
	if len(p.Extra) > 0 {
		req.Extra = mergeExtra(p.Extra, req.Extra)
	}
	var over http.Header
	for _, d := range p.Directives {
		if len(d.Extra) > 0 {
			req.Extra = mergeExtra(req.Extra, d.Extra)
		}
		for k, vs := range d.Header {
			if over == nil {
				over = http.Header{}
			}
			over[k] = append([]string(nil), vs...)
		}
	}
	call, err := p.Protocol.Encode(req)
	if err != nil {
		return nil, err
	}
	return p.send(ctx, call, over)
}

// Issues any call against the endpoint with auth and retries, returning the 2xx response unread
func (p *Provider) Do(ctx context.Context, call protocol.Call) (*http.Response, error) {
	return p.send(ctx, call, nil)
}

// Issues the call with the headers in over laid above the static ones, with auth and retries, returning the 2xx response unread
func (p *Provider) send(ctx context.Context, call protocol.Call, over http.Header) (*http.Response, error) {
	if p.Protocol == nil {
		return nil, errors.New("provider: no protocol")
	}
	target, err := p.target(call)
	if err != nil {
		return nil, err
	}
	label := LabelFrom(ctx)
	for attempt := 1; ; attempt++ {
		if r, ok := p.Auth.(Refresher); ok {
			if err := r.Refresh(ctx); err != nil {
				return nil, fmt.Errorf("provider %s: %w", p.Name, err)
			}
		}
		at := Attempt{URL: target, Label: label, Started: time.Now()}
		p.begin(at)
		resp, err := p.issue(ctx, call, over, at)
		at.Duration = time.Since(at.Started)
		if err == nil {
			at.Status = resp.StatusCode
			at.RequestID = RequestID(resp.Header)
			at.RateLimit = ParseRateLimit(resp.Header)
		}
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		var failure error
		if err != nil {
			failure = err
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			perr := p.Protocol.DecodeError(resp.StatusCode, body)
			failure = perr
			at.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
			if perr.Stuck() || !retryable(resp.StatusCode) && !perr.Transient {
				at.Err = failure
				p.observe(at)
				return nil, failure
			}
		}
		at.Err = failure
		var stall *StallError
		if ctx.Err() != nil || attempt >= retryAttempts || errors.As(failure, &stall) {
			p.observe(at)
			return nil, failure
		}
		wait := at.RetryAfter
		if wait == 0 {
			wait = backoff(attempt)
		}
		wait = min(wait, max(retryMax, time.Minute))
		at.Wait = wait
		p.observe(at)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("provider %s: %w after %d attempts, last failure: %v", p.Name, ctx.Err(), attempt, failure)
		case <-time.After(wait):
		}
	}
}

// Sends the call once, giving up when the headers do not come in time, and bounds the silence of a body it hands back,
// which reports the attempt once it is read to the end or closed
func (p *Provider) issue(ctx context.Context, call protocol.Call, over http.Header, at Attempt) (*http.Response, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	hr, err := http.NewRequestWithContext(rctx, call.Method, at.URL, bytes.NewReader(call.Body))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	p.decorate(hr, call.Header, over)
	wait := p.headerTimeout()
	headers := time.AfterFunc(wait, func() { cancel(&StallError{Provider: p.Name, Headers: true, After: wait}) })
	resp, err := client.Do(hr)
	headers.Stop()
	if err != nil {
		var stall *StallError
		if errors.As(context.Cause(rctx), &stall) {
			cancel(nil)
			return nil, stall
		}
		cancel(nil)
		return nil, fmt.Errorf("provider %s: %w", p.Name, err)
	}
	if resp.StatusCode/100 != 2 {
		resp.Body = &released{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	at.Status, at.RequestID, at.RateLimit = resp.StatusCode, RequestID(resp.Header), ParseRateLimit(resp.Header)
	b := &bounded{ReadCloser: resp.Body, p: p, ctx: rctx, cancel: cancel, stall: p.stallTimeout(), at: at}
	b.arm()
	resp.Body = b
	return resp, nil
}

func (p *Provider) observe(a Attempt) {
	if p.Observer != nil {
		p.Observer.Observe(a)
	}
}

// Body of a failed response that releases the request context once closed
type released struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (r *released) Close() error {
	r.cancel(nil)
	return r.ReadCloser.Close()
}

// Body that cancels its request once no byte arrived within the stall bound and reports the attempt when it ends
type bounded struct {
	io.ReadCloser
	p      *Provider
	ctx    context.Context
	cancel context.CancelCauseFunc
	stall  time.Duration
	at     Attempt
	mu     sync.Mutex
	timer  *time.Timer
	once   sync.Once
}

// Starts the silence clock afresh
func (b *bounded) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.stall, func() { b.cancel(&StallError{Provider: b.p.Name, After: b.stall}) })
}

func (b *bounded) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	if n > 0 {
		b.arm()
	}
	if err == io.EOF {
		b.finish(nil)
	} else if err != nil {
		if cause := context.Cause(b.ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
		b.finish(err)
	}
	return n, err
}

func (b *bounded) Close() error {
	b.finish(nil)
	return b.ReadCloser.Close()
}

// Reports the attempt with its whole duration once, then releases the request
func (b *bounded) finish(err error) {
	b.once.Do(func() {
		b.mu.Lock()
		if b.timer != nil {
			b.timer.Stop()
		}
		b.mu.Unlock()
		b.at.Duration = time.Since(b.at.Started)
		b.at.Err = err
		b.p.observe(b.at)
		b.cancel(nil)
	})
}

func (p *Provider) begin(a Attempt) {
	if s, ok := p.Observer.(Starter); ok {
		s.Begin(a)
	}
}

// Lays request extras over provider extras, merging nested objects and replacing everything else
func mergeExtra(base, over map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]json.RawMessage, len(over))
	}
	for k, v := range over {
		var bo, vo map[string]json.RawMessage
		if b, ok := out[k]; ok && json.Unmarshal(b, &bo) == nil && bo != nil && json.Unmarshal(v, &vo) == nil && vo != nil {
			if merged, err := json.Marshal(mergeExtra(bo, vo)); err == nil {
				out[k] = merged
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (p *Provider) target(call protocol.Call) (string, error) {
	base, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("provider %s: invalid base url %q", p.Name, p.BaseURL)
	}
	base.Path += call.Path
	values := base.Query()
	for k, vs := range p.Query {
		values[k] = vs
	}
	for k, vs := range call.Query {
		values[k] = vs
	}
	base.RawQuery = values.Encode()
	return base.String(), nil
}

// Layers protocol headers, static provider headers, directive headers and the credential onto the request
func (p *Provider) decorate(r *http.Request, protocolHeader, over http.Header) {
	for _, h := range []http.Header{protocolHeader, p.Header, over} {
		for k, vs := range h {
			r.Header[k] = append([]string(nil), vs...)
		}
	}
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", UserAgent)
	}
	if p.Auth != nil {
		p.Auth.Apply(r)
	}
}

// Sent when neither the protocol nor the provider names a user agent
const UserAgent = "icebath"

// Reports whether a status is worth retrying, matching the official SDK retry set
func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500
}

// Parses a Retry-After header given in seconds or as an HTTP date
func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

// Doubles the base per attempt, caps at the maximum and keeps between half and the full value
func backoff(attempt int) time.Duration {
	d := min(retryBase<<(attempt-1), retryMax)
	return time.Duration(float64(d) * (0.5 + rand.Float64()/2))
}

// What the endpoint reported about its quota on one response, zero where it said nothing
type RateLimit struct {
	Requests          int
	RequestsRemaining int
	Tokens            int
	TokensRemaining   int
	InputTokens       int
	InputRemaining    int
	OutputTokens      int
	OutputRemaining   int
	Reset             time.Duration
}

// Reports whether any field was filled in
func (r RateLimit) Known() bool { return r != RateLimit{} }

// One HTTP attempt of a call with what came back and how long the provider will wait before the next
type Attempt struct {
	URL string
	// Label the caller set on the context, such as the run the call serves
	Label    string
	Started  time.Time
	Duration time.Duration
	Status   int
	// Transport or decoded wire failure, nil on a 2xx
	Err error
	// Pause before the next attempt, zero when this one was the last
	Wait time.Duration
	// Pause the endpoint asked for through Retry-After
	RetryAfter time.Duration
	RequestID  string
	RateLimit  RateLimit
}

// Reports whether the attempt returned a usable response
func (a Attempt) OK() bool { return a.Err == nil && a.Status/100 == 2 }

// Receives every attempt a provider makes
type Observer interface {
	Observe(Attempt)
}

// Observer that also hears each attempt as it starts, before anything comes back
type Starter interface {
	Begin(Attempt)
}

// Header names vendors use for the request identifier, in lookup order
var requestIDHeaders = []string{"request-id", "x-request-id", "x-amzn-requestid", "x-amz-request-id", "cf-ray", "x-goog-request-id"}

// Request identifier named by any of the known headers
func RequestID(h http.Header) string {
	for _, name := range requestIDHeaders {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// Reads the rate limit headers of every supported vendor into one view
func ParseRateLimit(h http.Header) RateLimit {
	var r RateLimit
	get := func(names ...string) int {
		for _, n := range names {
			if v := h.Get(n); v != "" {
				if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					return i
				}
			}
		}
		return 0
	}
	r.Requests = get("x-ratelimit-limit-requests", "anthropic-ratelimit-requests-limit", "x-ratelimit-limit")
	r.RequestsRemaining = get("x-ratelimit-remaining-requests", "anthropic-ratelimit-requests-remaining", "x-ratelimit-remaining")
	r.Tokens = get("x-ratelimit-limit-tokens", "anthropic-ratelimit-tokens-limit")
	r.TokensRemaining = get("x-ratelimit-remaining-tokens", "anthropic-ratelimit-tokens-remaining")
	r.InputTokens = get("anthropic-ratelimit-input-tokens-limit")
	r.InputRemaining = get("anthropic-ratelimit-input-tokens-remaining")
	r.OutputTokens = get("anthropic-ratelimit-output-tokens-limit")
	r.OutputRemaining = get("anthropic-ratelimit-output-tokens-remaining")
	for _, n := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset", "x-ratelimit-reset"} {
		if d := parseReset(h.Get(n)); d > 0 {
			r.Reset = d
			break
		}
	}
	return r
}

// Reads a reset given as seconds, a duration such as 6m0s, or an RFC 3339 time
func parseReset(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second))
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	if at, err := time.Parse(time.RFC3339, v); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}
