package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
)

// Credential is one usable login for a provider
type Credential struct {
	APIKey      string
	TokenSource func(ctx context.Context) (string, error)
	BaseURL     string
	Source      string
}

// CredentialSource answers which login a provider has
type CredentialSource interface {
	Credential(provider string) (Credential, bool)
}

// Overrides are the explicit CLI or runtime request that beats every config and env rung
type Overrides struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	Mode     APIMode
	// Forwarded token minter standing in for a static key, such as an Entra ID bearer
	TokenSource func(ctx context.Context) (string, error)
	// Parse failure of config.yaml that blocks env key auto adoption
	ConfigError error
}

// Resolve walks the hermes ladder from explicit request through config, env keys, pooled logins, and auto detection
func Resolve(ctx context.Context, cfg config.Tree, secrets *config.Secrets, home string, creds CredentialSource, over Overrides) (Route, error) {
	return resolveRuntimeLadder(ctx, cfg, secrets, home, creds, over)
}

// ResolveAux picks the auxiliary route for a task from config auxiliary with fallback to the main route
func ResolveAux(ctx context.Context, cfg config.Tree, secrets *config.Secrets, home string, creds CredentialSource, task string) (Route, error) {
	return resolveAuxiliaryRoute(ctx, cfg, secrets, home, creds, task)
}

// FallbackRoutes resolves every fallback_providers entry into a route in chain order
func FallbackRoutes(ctx context.Context, cfg config.Tree, secrets *config.Secrets, home string, creds CredentialSource) ([]Route, error) {
	return resolveFallbackChain(ctx, cfg, secrets, home, creds)
}
