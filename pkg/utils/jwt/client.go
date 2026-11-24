package jwtutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Error of a key lookup the JWK set has no signing key for, wrapped with the key id asked for
var ErrKeyNotFound = errors.New("jwk set has no matching signing key")

// Error of a JWK set that holds no key able to verify signatures
var ErrNoKeys = errors.New("jwk set has no usable signing keys")

// Error of a token whose signing algorithm differs from the one its key is published for
var ErrAlgorithm = errors.New("token algorithm does not match its key")

const (
	defaultTimeout    = 30 * time.Second
	defaultLifespan   = 5 * time.Minute
	defaultMinRefresh = 30 * time.Second
	maxSetBytes       = 1 << 20
)

// Failed fetch of a JWK set: its address, the HTTP status when the server answered with an error status, else the transport or decoding error
type FetchError struct {
	URL        string
	StatusCode int
	Err        error
}

// The address and what went wrong, as in fetch jwk set https://example.com/keys: 503 Service Unavailable
func (e *FetchError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("fetch jwk set %s: %d %s", e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("fetch jwk set %s: %v", e.URL, e.Err)
}

// Transport or decoding error of the fetch, nil for an error status
func (e *FetchError) Unwrap() error { return e.Err }

// Cached view of a JWK set endpoint, safe for concurrent use: Header is sent with each fetch, HTTP fetches (http.DefaultClient when nil), Timeout bounds one fetch (30 seconds when zero), Lifespan is how long a fetched set serves before a lookup renews it in the background (5 minutes when zero), MinRefresh is the shortest gap after which an unknown key id or a failed refresh fetches again (30 seconds when zero); set the fields before the first lookup
type JWKClient struct {
	URL        string
	Header     http.Header
	HTTP       *http.Client
	Timeout    time.Duration
	Lifespan   time.Duration
	MinRefresh time.Duration

	mu      sync.Mutex
	keys    []*Key
	fetched time.Time
	failed  time.Time
	pending *fetchCall
}

// One fetch of the set that every lookup arriving meanwhile waits for
type fetchCall struct {
	done chan struct{}
	err  error
}

// Client of the JWK set at url with the default timeout, lifespan and refresh gap
func NewJWKClient(url string) *JWKClient { return &JWKClient{URL: url} }

// Signing key with key id kid, or the only signing key of the set when kid is empty and the set has exactly one; the cached set answers at once, past its lifespan too while a background fetch renews it or after that fetch failed, and an unknown kid waits for a fetch of the set once MinRefresh has passed since the last one
func (c *JWKClient) Key(ctx context.Context, kid string) (*Key, error) {
	var key *Key
	err := c.settle(ctx, func(keys []*Key) bool {
		key = pick(keys, kid)
		return key != nil
	})
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, notFound(kid)
	}
	return key, nil
}

// Every signing key of the set, cached and refreshed as Key does
func (c *JWKClient) Keys(ctx context.Context) ([]*Key, error) {
	var out []*Key
	err := c.settle(ctx, func(keys []*Key) bool {
		out = keys
		return len(keys) > 0
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoKeys
	}
	return slices.Clone(out), nil
}

// Key function for jwt parsing: the public key the token's kid header names, every signing key that fits the token's algorithm when the kid header is absent or empty, an error for a key published for another algorithm or of a type the algorithm cannot verify with
func (c *JWKClient) Keyfunc(ctx context.Context) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		alg := ""
		if t.Method != nil {
			alg = t.Method.Alg()
		}
		v, named := t.Header["kid"]
		kid, isText := v.(string)
		if named && v != nil && !isText {
			return nil, fmt.Errorf("%w: kid header is %T, not a string", ErrKeyNotFound, v)
		}
		if kid == "" {
			keys, err := c.Keys(ctx)
			if err != nil {
				return nil, err
			}
			set := jwt.VerificationKeySet{}
			for _, k := range keys {
				if (k.Algorithm == "" || k.Algorithm == alg) && fits(t.Method, k.Public) {
					set.Keys = append(set.Keys, k.Public)
				}
			}
			if len(set.Keys) == 0 {
				return nil, fmt.Errorf("%w: token names no key id and no key is published for %s", ErrKeyNotFound, alg)
			}
			return set, nil
		}
		key, err := c.Key(ctx, kid)
		if err != nil {
			return nil, err
		}
		if key.Algorithm != "" && key.Algorithm != alg {
			return nil, fmt.Errorf("%w: key %q is for %s, token uses %s", ErrAlgorithm, key.ID, key.Algorithm, alg)
		}
		if !fits(t.Method, key.Public) {
			return nil, fmt.Errorf("%w: key %q is a %T, which %s does not verify with", ErrAlgorithm, key.ID, key.Public, alg)
		}
		return key.Public, nil
	}
}

// Runs match over the cached set, after a fetch when there is no set yet or when match finds nothing in a set older than MinRefresh; nil once match has run on the newest set it may see, the fetch error when no set satisfies it
func (c *JWKClient) settle(ctx context.Context, match func([]*Key) bool) error {
	call := c.check(ctx, match)
	if call == nil {
		return nil
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if match(c.keys) {
		return nil
	}
	return call.err
}

// Nil when the cache answers match as it stands, a set past its lifespan included while a background fetch renews it, else the fetch to wait for, started here when none is running
func (c *JWKClient) check(ctx context.Context, match func([]*Key) bool) *fetchCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	recentlyFailed := !c.failed.IsZero() && now.Sub(c.failed) < c.minRefresh()
	if c.keys != nil && match(c.keys) {
		if now.Sub(c.fetched) >= c.lifespan() && c.pending == nil && !recentlyFailed {
			c.start(ctx)
		}
		return nil
	}
	if c.pending != nil {
		return c.pending
	}
	if c.keys != nil && (recentlyFailed || now.Sub(c.fetched) < c.minRefresh()) {
		return nil
	}
	return c.start(ctx)
}

// Fetch of the set run on its own goroutine, bounded by Timeout and not by the caller's cancellation; the caller holds mu
func (c *JWKClient) start(ctx context.Context) *fetchCall {
	call := &fetchCall{done: make(chan struct{})}
	c.pending = call
	go c.run(context.WithoutCancel(ctx), call)
	return call
}

// Fetches the set for one pending call, installing it on success and stamping the failure otherwise
func (c *JWKClient) run(ctx context.Context, call *fetchCall) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	keys, err := c.fetch(ctx)
	cancel()
	c.mu.Lock()
	if err == nil {
		c.keys, c.fetched, c.failed = keys, time.Now(), time.Time{}
	} else {
		c.failed = time.Now()
	}
	call.err = err
	c.pending = nil
	c.mu.Unlock()
	close(call.done)
}

// Signing keys of the set at URL, ErrNoKeys when it holds none
func (c *JWKClient) fetch(ctx context.Context) ([]*Key, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, &FetchError{URL: c.URL, Err: err}
	}
	for name, values := range c.Header {
		req.Header[name] = values
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &FetchError{URL: c.URL, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxSetBytes))
		return nil, &FetchError{URL: c.URL, StatusCode: resp.StatusCode}
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSetBytes)).Decode(&doc); err != nil {
		return nil, &FetchError{URL: c.URL, StatusCode: resp.StatusCode, Err: err}
	}
	keys := make([]*Key, 0, len(doc.Keys))
	for _, raw := range doc.Keys {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if k, ok := parseKey(m); ok && canVerify(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w at %s", ErrNoKeys, c.URL)
	}
	return keys, nil
}

// Key with id kid, the only key when kid is empty and there is exactly one, nil otherwise
func pick(keys []*Key, kid string) *Key {
	if kid == "" {
		if len(keys) == 1 {
			return keys[0]
		}
		return nil
	}
	for _, k := range keys {
		if k.ID == kid {
			return k
		}
	}
	return nil
}

func notFound(kid string) error {
	if kid == "" {
		return fmt.Errorf("%w: no key id given and the set does not hold exactly one key", ErrKeyNotFound)
	}
	return fmt.Errorf("%w: kid %q", ErrKeyNotFound, kid)
}

func (c *JWKClient) timeout() time.Duration { return orDefault(c.Timeout, defaultTimeout) }

func (c *JWKClient) lifespan() time.Duration { return orDefault(c.Lifespan, defaultLifespan) }

func (c *JWKClient) minRefresh() time.Duration { return orDefault(c.MinRefresh, defaultMinRefresh) }

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
