package connections

import (
	"context"
	"errors"
	"sync"
	"time"
)

// NativeAccessTokenOptions force a refresh, optionally only when the rejected token is still the stored one
type NativeAccessTokenOptions struct {
	ForceRefresh        bool
	RejectedAccessToken string
}

// NativeAuthChangedError is a login or logout that changed the identity under a request in flight
type NativeAuthChangedError struct{}

func (NativeAuthChangedError) Error() string {
	return "Authentication changed while the request was in progress. Try again."
}

func isNativeAuthChanged(err error) bool {
	var changed NativeAuthChangedError
	return errors.As(err, &changed)
}

// NativeAccessTokenDeps are the token store and refresh exchange the coordinator runs over
type NativeAccessTokenDeps struct {
	ClearTokens            func(baseURL string)
	IsRefreshAuthRejection func(err error) bool
	LoadTokens             func(baseURL string) *NativeTokenSet
	NormalizeBaseURL       func(baseURL string) (string, error)
	RefreshTokens          func(ctx context.Context, baseURL string, tokens NativeTokenSet) (NativeTokenSet, error)
	StoreTokens            func(baseURL string, tokens NativeTokenSet) error
	TokenNeedsRefresh      func(tokens NativeTokenSet, nowSeconds float64) bool
}

type refreshFlight struct {
	done  chan struct{}
	token *string
	err   error
}

// NativeAccessTokenCoordinator single flights refresh rotations per gateway and fences them against auth changes
type NativeAccessTokenCoordinator struct {
	deps        NativeAccessTokenDeps
	mu          sync.Mutex
	flights     map[string]*refreshFlight
	authEpochs  map[string]int
	loginEpochs map[string]int
}

// NewNativeAccessTokenCoordinator builds a coordinator over its store and refresh dependencies
func NewNativeAccessTokenCoordinator(deps NativeAccessTokenDeps) *NativeAccessTokenCoordinator {
	return &NativeAccessTokenCoordinator{deps: deps, flights: map[string]*refreshFlight{}, authEpochs: map[string]int{}, loginEpochs: map[string]int{}}
}

func (c *NativeAccessTokenCoordinator) beginLoginLocked(baseURL string) func() bool {
	epoch := c.loginEpochs[baseURL] + 1
	c.loginEpochs[baseURL] = epoch
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.loginEpochs[baseURL] == epoch
	}
}

// BeginLogin starts a login attempt and reports whether it still owns the gateway auth
func (c *NativeAccessTokenCoordinator) BeginLogin(rawBaseURL string) (func() bool, error) {
	baseURL, err := c.deps.NormalizeBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.beginLoginLocked(baseURL), nil
}

func (c *NativeAccessTokenCoordinator) beginExplicitAuthChangeLocked(baseURL string) {
	c.beginLoginLocked(baseURL)
	c.authEpochs[baseURL]++
	delete(c.flights, baseURL)
}

// Ensure is a valid access token for the gateway, rotating a stale one, nil without tokens or after a dead refresh
func (c *NativeAccessTokenCoordinator) Ensure(ctx context.Context, rawBaseURL string, o NativeAccessTokenOptions) (*string, error) {
	baseURL, err := c.deps.NormalizeBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if flight := c.flights[baseURL]; flight != nil {
		c.mu.Unlock()
		<-flight.done
		return flight.token, flight.err
	}
	c.mu.Unlock()
	tokens := c.deps.LoadTokens(baseURL)
	if tokens == nil {
		return nil, nil
	}
	now := float64(time.Now().Unix())
	rejectedCurrent := o.RejectedAccessToken == "" || o.RejectedAccessToken == tokens.AccessToken
	if !(o.ForceRefresh && rejectedCurrent) && !c.deps.TokenNeedsRefresh(*tokens, now) {
		token := tokens.AccessToken
		return &token, nil
	}
	if tokens.RefreshToken == "" {
		c.deps.ClearTokens(baseURL)
		return nil, nil
	}
	c.mu.Lock()
	if flight := c.flights[baseURL]; flight != nil {
		c.mu.Unlock()
		<-flight.done
		return flight.token, flight.err
	}
	flightEpoch := c.authEpochs[baseURL]
	flight := &refreshFlight{done: make(chan struct{})}
	c.flights[baseURL] = flight
	c.mu.Unlock()
	current := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.authEpochs[baseURL] == flightEpoch
	}
	flight.token, flight.err = func() (*string, error) {
		rotated, err := c.deps.RefreshTokens(ctx, baseURL, *tokens)
		if err != nil {
			if !current() {
				return nil, NativeAuthChangedError{}
			}
			if c.deps.IsRefreshAuthRejection(err) {
				c.deps.ClearTokens(baseURL)
				return nil, nil
			}
			return nil, err
		}
		if !current() {
			return nil, NativeAuthChangedError{}
		}
		if err := c.deps.StoreTokens(baseURL, rotated); err != nil {
			return nil, err
		}
		token := rotated.AccessToken
		return &token, nil
	}()
	close(flight.done)
	c.mu.Lock()
	if c.flights[baseURL] == flight {
		delete(c.flights, baseURL)
	}
	c.mu.Unlock()
	return flight.token, flight.err
}

// StoreTokens records a confirmed login's tokens, fencing every refresh in flight
func (c *NativeAccessTokenCoordinator) StoreTokens(rawBaseURL string, tokens NativeTokenSet) error {
	baseURL, err := c.deps.NormalizeBaseURL(rawBaseURL)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.beginExplicitAuthChangeLocked(baseURL)
	c.mu.Unlock()
	return c.deps.StoreTokens(baseURL, tokens)
}

// ClearTokens drops a gateway's tokens, fencing every refresh in flight
func (c *NativeAccessTokenCoordinator) ClearTokens(rawBaseURL string) error {
	baseURL, err := c.deps.NormalizeBaseURL(rawBaseURL)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.beginExplicitAuthChangeLocked(baseURL)
	c.mu.Unlock()
	c.deps.ClearTokens(baseURL)
	return nil
}

// Decrypted native token cache over the encrypted store, keyed by normalized gateway URL
type nativeTokenCache struct {
	mu     sync.Mutex
	tokens map[string]NativeTokenSet
}

func (s *Service) loadNativeTokens(baseURL string) *NativeTokenSet {
	normalized, err := NormalizeRemoteBaseURL(baseURL)
	if err != nil {
		return nil
	}
	cache := &s.auth.state().native
	cache.mu.Lock()
	if cached, ok := cache.tokens[normalized]; ok {
		cache.mu.Unlock()
		return &cached
	}
	cache.mu.Unlock()
	tokens := LoadNativeTokenSet(normalized, s.nativeTokenStoreIO())
	if tokens != nil {
		cache.mu.Lock()
		cache.tokens[normalized] = *tokens
		cache.mu.Unlock()
	}
	return tokens
}

// Persists and caches a token set, a failed encryption leaving both untouched
func (s *Service) storeNativeTokens(baseURL string, tokens NativeTokenSet) error {
	if err := PersistNativeTokenSet(baseURL, &tokens, s.nativeTokenStoreIO()); err != nil {
		return err
	}
	cache := &s.auth.state().native
	cache.mu.Lock()
	cache.tokens[baseURL] = tokens
	cache.mu.Unlock()
	return nil
}

func (s *Service) clearNativeTokens(baseURL string) {
	cache := &s.auth.state().native
	cache.mu.Lock()
	delete(cache.tokens, baseURL)
	cache.mu.Unlock()
	_ = PersistNativeTokenSet(baseURL, nil, s.nativeTokenStoreIO())
}

// Whether native bearer tokens are held for the gateway
func (s *Service) hasNativeSession(baseURL string) bool {
	return s.loadNativeTokens(baseURL) != nil
}

// Coordinator wired to the encrypted store and the native refresh endpoint
func (s *Service) newNativeAccessTokenCoordinator() *NativeAccessTokenCoordinator {
	return NewNativeAccessTokenCoordinator(NativeAccessTokenDeps{
		ClearTokens:            s.clearNativeTokens,
		IsRefreshAuthRejection: func(err error) bool { return ReadStatusCode(err) == 401 },
		LoadTokens:             s.loadNativeTokens,
		NormalizeBaseURL:       func(raw string) (string, error) { return NormalizeRemoteBaseURL(raw) },
		RefreshTokens: func(ctx context.Context, baseURL string, tokens NativeTokenSet) (NativeTokenSet, error) {
			refreshURL, err := NativeRefreshURL(baseURL)
			if err != nil {
				return NativeTokenSet{}, err
			}
			body, err := s.postJSONNoAuth(ctx, refreshURL, map[string]any{"refresh_token": tokens.RefreshToken, "provider": tokens.Provider}, JSONOptions{TimeoutMs: 10_000})
			if err != nil {
				return NativeTokenSet{}, err
			}
			return ParseTokenResponse(body)
		},
		StoreTokens:       s.storeNativeTokens,
		TokenNeedsRefresh: func(tokens NativeTokenSet, now float64) bool { return TokenNeedsRefresh(tokens.ExpiresAt, now, 60) },
	})
}
