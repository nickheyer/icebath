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

// Attempt count and exponential backoff bounds for retryable failures
type Retry struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

var DefaultRetry = Retry{Attempts: 3, Base: 500 * time.Millisecond, Max: 8 * time.Second}

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
	// How the endpoint hears a turn asking it to keep nothing
	Ghost Directive
	// How the endpoint hears a turn asking it to add no prompt or tools of its own
	Bare   Directive
	Client *http.Client
	Retry  Retry
	// Receives every attempt with its status, headers and backoff
	Observer Observer
}

// Sends one turn without streaming and decodes the complete response
func (p *Provider) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	req.Stream = false
	resp, err := p.do(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	defer resp.Body.Close()
	out, err := p.Protocol.Decode(resp.Body)
	if err != nil {
		return protocol.Response{}, err
	}
	stamp(&out, resp.Header)
	return out, nil
}

// Keeps the response headers and request id on a decoded response
func stamp(r *protocol.Response, h http.Header) {
	r.Header = h.Clone()
	r.RequestID = RequestID(h)
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
			if err == nil && ev.Type == protocol.EventDone {
				stamp(&ev.Response, resp.Header)
			}
			if !yield(ev, err) || err != nil {
				return
			}
		}
	}
}

// Encodes the request with provider extras and the directives the context asks for, then issues it
func (p *Provider) do(ctx context.Context, req protocol.Request) (*http.Response, error) {
	if p.Protocol == nil {
		return nil, errors.New("provider: no protocol")
	}
	if len(p.Extra) > 0 {
		req.Extra = mergeExtra(p.Extra, req.Extra)
	}
	var over http.Header
	for _, d := range p.asked(protocol.DirectivesFrom(ctx)) {
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

// How the endpoint hears each directive asked for
func (p *Provider) asked(d protocol.Directives) []Directive {
	var out []Directive
	if d.Ghost {
		out = append(out, p.Ghost)
	}
	if d.Bare {
		out = append(out, p.Bare)
	}
	return out
}

// Issues any call against the endpoint with auth and retries, returning the 2xx response unread
func (p *Provider) Do(ctx context.Context, call protocol.Call) (*http.Response, error) {
	return p.send(ctx, call, nil)
}

// Issues the call with the headers in over laid above the static ones
func (p *Provider) send(ctx context.Context, call protocol.Call, over http.Header) (*http.Response, error) {
	if p.Protocol == nil {
		return nil, errors.New("provider: no protocol")
	}
	target, err := p.target(call)
	if err != nil {
		return nil, err
	}
	retry := p.Retry
	if retry.Attempts <= 0 {
		retry = DefaultRetry
	}
	if retry.Base <= 0 {
		retry.Base = DefaultRetry.Base
	}
	if retry.Max <= 0 {
		retry.Max = DefaultRetry.Max
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	for attempt := 1; ; attempt++ {
		if r, ok := p.Auth.(Refresher); ok {
			if err := r.Refresh(ctx); err != nil {
				return nil, fmt.Errorf("provider %s: %w", p.Name, err)
			}
		}
		hr, err := http.NewRequestWithContext(ctx, call.Method, target, bytes.NewReader(call.Body))
		if err != nil {
			return nil, err
		}
		p.decorate(hr, call.Header, over)
		at := Attempt{Provider: p.Name, Attempt: attempt, Method: call.Method, URL: target, Started: time.Now()}
		p.begin(at)
		resp, err := client.Do(hr)
		at.Duration = time.Since(at.Started)
		if err == nil {
			at.Status = resp.StatusCode
			at.Header = resp.Header.Clone()
			at.RequestID = RequestID(resp.Header)
			at.RateLimit = ParseRateLimit(resp.Header)
		}
		if err == nil && resp.StatusCode/100 == 2 {
			resp.Body = &timed{ReadCloser: resp.Body, at: at, p: p}
			return resp, nil
		}
		var failure error
		if err != nil {
			failure = fmt.Errorf("provider %s: %w", p.Name, err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			failure = p.Protocol.DecodeError(resp.StatusCode, body)
			at.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
		}
		at.Err = failure
		if err == nil && !retryable(resp.StatusCode) {
			p.observe(at)
			return nil, failure
		}
		if ctx.Err() != nil || attempt >= retry.Attempts {
			p.observe(at)
			return nil, failure
		}
		wait := at.RetryAfter
		if wait == 0 {
			wait = backoff(retry, attempt)
		}
		wait = min(wait, maxWait(retry))
		at.Wait = wait
		p.observe(at)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("provider %s: %w after %d attempts, last failure: %v", p.Name, ctx.Err(), attempt, failure)
		case <-time.After(wait):
		}
	}
}

func (p *Provider) observe(a Attempt) {
	if p.Observer != nil {
		p.Observer.Observe(a)
	}
}

func (p *Provider) begin(a Attempt) {
	if s, ok := p.Observer.(Starter); ok {
		s.Begin(a)
	}
}

// Response body that reports the attempt with its whole duration once read to the end or closed
type timed struct {
	io.ReadCloser
	at   Attempt
	p    *Provider
	once sync.Once
}

func (b *timed) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *timed) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *timed) finish() {
	b.once.Do(func() {
		b.at.Duration = time.Since(b.at.Started)
		b.p.observe(b.at)
	})
}

// Longest pause honored between attempts, at least a minute so Retry-After may stretch past a shorter retry maximum
func maxWait(r Retry) time.Duration {
	return max(r.Max, time.Minute)
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

// Reports whether an error is worth retrying, a transport failure or a retryable status
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return retryable(perr.Status)
	}
	return true
}

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
func backoff(r Retry, attempt int) time.Duration {
	d := r.Base << (attempt - 1)
	if r.Max > 0 && d > r.Max {
		d = r.Max
	}
	return time.Duration(float64(d) * (0.5 + rand.Float64()/2))
}
