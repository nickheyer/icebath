package provider

import (
	"context"
	"fmt"
	"sync"
)

// Process wide set of keys already logged so repeated resolutions stay silent
type auxOnceSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newAuxOnceSet() *auxOnceSet { return &auxOnceSet{seen: map[string]bool{}} }

// Adds the key and reports whether it was new
func (s *auxOnceSet) add(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[key] {
		return false
	}
	s.seen[key] = true
	return true
}

// Reports whether the key was already logged
func (s *auxOnceSet) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[key]
}

// Forgets every logged key as a test reset does
func (s *auxOnceSet) clear() {
	s.mu.Lock()
	s.seen = map[string]bool{}
	s.mu.Unlock()
}

// Per branch dedup of resolve_provider_client fall through warnings
var (
	auxLoggedUnknownProviderKeys    = newAuxOnceSet()
	auxLoggedUnhandledAuthTypeKeys  = newAuxOnceSet()
	auxLoggedUnsupportedExtProcKeys = newAuxOnceSet()
	auxLoggedUnsupportedOAuthKeys   = newAuxOnceSet()
)

// AuxChatStream yields OpenAI shaped chunks until io.EOF
type AuxChatStream interface {
	Next(ctx context.Context) (map[string]any, error)
	Close() error
}

// Default substantive progress window of a streamed auxiliary call in seconds
var auxStreamNoProgressTimeoutSeconds = 60.0

type auxProbeKey struct{}

// AuxProbeMode resolves provider availability under the returned context without building real SDK clients
func AuxProbeMode(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, auxProbeKey{}, true)
}

// True inside an availability probe scope
func auxProbeActive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	active, _ := ctx.Value(auxProbeKey{}).(bool)
	return active
}

// Client returned during an availability probe that fails loudly on any runtime use
type auxProbeClient struct {
	auxEffectiveStamp
	apiKey  string
	baseURL string
}

func (c *auxProbeClient) BaseURL() string { return c.baseURL }
func (c *auxProbeClient) APIKey() string  { return c.apiKey }
func (c *auxProbeClient) String() string  { return "<aux availability-probe client stub>" }

func (c *auxProbeClient) used(name string) error {
	return &AuxRuntimeError{Message: fmt.Sprintf("_AuxProbeClientStub used as a real client (attribute '%s'); "+
		"aux_probe_mode is for availability checks only", name)}
}

func (c *auxProbeClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, c.used("chat")
}

func (c *auxProbeClient) Close() error { return c.used("close") }

// Availability probe clients refuse the Responses call like every other real client use
func (c *auxProbeClient) CreateResponse(context.Context, map[string]any) (any, error) {
	return nil, c.used("responses")
}
