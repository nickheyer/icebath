package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// UpstreamCredential is a resolved bearer and base URL ready to forward to
type UpstreamCredential struct {
	Bearer    string
	BaseURL   string
	TokenType string
	ExpiresAt string
}

// Authorization scheme of the credential, Bearer unless the adapter named another
func (c UpstreamCredential) tokenType() string {
	if c.TokenType == "" {
		return "Bearer"
	}
	return c.TokenType
}

// UpstreamAdapter is the contract for an upstream provider the proxy can forward to
type UpstreamAdapter interface {
	Name() string
	DisplayName() string
	AllowedPaths() map[string]bool
	IsAuthenticated() bool
	GetCredential() (UpstreamCredential, error)
	GetRetryCredential(failed UpstreamCredential, statusCode int) (*UpstreamCredential, error)
}

// Adapter naming the login command proxy start prints when it is not authenticated
type upstreamAuthHinter interface {
	AuthHint() string
}

// One line status summary of an adapter for proxy status
func describeAdapter(a UpstreamAdapter) string {
	cred, err := a.GetCredential()
	if err != nil {
		return fmt.Sprintf("%s: not ready (%s)", a.DisplayName(), pyrepr.ExceptionMessage(err))
	}
	ttl := ""
	if cred.ExpiresAt != "" {
		ttl = fmt.Sprintf(" (expires %s)", cred.ExpiresAt)
	}
	return fmt.Sprintf("%s: %s%s", a.DisplayName(), cred.BaseURL, ttl)
}

// Adapter constructors keyed by the icebath proxy start --provider value
var proxyAdapters = map[string]func(env PoolEnv) UpstreamAdapter{
	"nous": func(env PoolEnv) UpstreamAdapter { return newNousPortalAdapter(env) },
	"xai":  func(env PoolEnv) UpstreamAdapter { return newXAIGrokAdapter(env) },
}

// Registered adapter names in sorted order
func proxyAdapterNames() []string {
	names := make([]string, 0, len(proxyAdapters))
	for name := range proxyAdapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Instantiates an adapter by provider name
func getAdapter(env PoolEnv, name string) (UpstreamAdapter, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	ctor, ok := proxyAdapters[key]
	if !ok {
		available := strings.Join(proxyAdapterNames(), ", ")
		if available == "" {
			available = "(none)"
		}
		return nil, fmt.Errorf("Unknown proxy upstream provider: %s. Available: %s", pyrepr.Repr(name), available)
	}
	return ctor(env), nil
}
