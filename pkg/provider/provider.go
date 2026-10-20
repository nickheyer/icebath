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
	Extra  map[string]json.RawMessage
	Client *http.Client
	Retry  Retry
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

// Encodes the request with the protocol and the provider extras and issues it
func (p *Provider) do(ctx context.Context, req protocol.Request) (*http.Response, error) {
	if p.Protocol == nil {
		return nil, errors.New("provider: no protocol")
	}
	if len(p.Extra) > 0 {
		req.Extra = mergeExtra(p.Extra, req.Extra)
	}
	call, err := p.Protocol.Encode(req)
	if err != nil {
		return nil, err
	}
	return p.Do(ctx, call)
}

// Issues any call against the endpoint with auth and retries, returning the 2xx response unread
func (p *Provider) Do(ctx context.Context, call protocol.Call) (*http.Response, error) {
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
		p.decorate(hr, call.Header)
		resp, err := client.Do(hr)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		var failure error
		var wait time.Duration
		if err != nil {
			failure = fmt.Errorf("provider %s: %w", p.Name, err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			failure = p.Protocol.DecodeError(resp.StatusCode, body)
			if !retryable(resp.StatusCode) {
				return nil, failure
			}
			wait = retryAfter(resp.Header.Get("Retry-After"))
		}
		if ctx.Err() != nil || attempt >= retry.Attempts {
			return nil, failure
		}
		if wait == 0 {
			wait = backoff(retry, attempt)
		}
		select {
		case <-ctx.Done():
			return nil, failure
		case <-time.After(wait):
		}
	}
}

// Lays request extras over provider extras, merging nested objects and replacing everything else
func mergeExtra(base, over map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(base)
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

// Layers protocol headers, static provider headers and the credential onto the request
func (p *Provider) decorate(r *http.Request, protocolHeader http.Header) {
	for k, vs := range protocolHeader {
		r.Header[k] = append([]string(nil), vs...)
	}
	for k, vs := range p.Header {
		r.Header[k] = append([]string(nil), vs...)
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
