package provider

import (
	"context"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// RuntimeCredentialSource reads an oauth provider's stored login the way the runtime credential resolvers do
type RuntimeCredentialSource interface {
	RuntimeCredential(provider string) (Credential, error)
}

// PoolPresenceSource reports whether a provider's credential pool holds any row at all
type PoolPresenceSource interface {
	HasPoolCredentials(provider string) bool
}

// Pool rows, auth store logins, the active login and the free tier one home's resolution reads
type homeCredentials struct {
	*PoolCredentials
	env PoolEnv
}

// Credential source for home h reading the pool, the auth store and the anon state as resolve_runtime_provider does
func homeCredentialSource(h string) CredentialSource {
	return newHomeCredentials(homePoolEnv(h))
}

// Exported credential source for home h so other packages resolve runtimes the way the provider does
func HomeCredentialSource(h string) CredentialSource { return homeCredentialSource(h) }

// Pool environment bound to the store, profile env file and config of home h
func homePoolEnv(h string) PoolEnv {
	return PoolEnv{Store: NewStore(h), Secrets: config.NewSecrets(h), Config: ConfigLoader(h)}
}

// Composite credential source over a pool environment, the store and secrets filled from the process home when unset
func newHomeCredentials(env PoolEnv) *homeCredentials {
	if env.Store == nil {
		h := home.Process()
		env.Store = NewStore(h)
		if env.Secrets == nil {
			env.Secrets = config.NewSecrets(h)
		}
		if env.Config == nil {
			env.Config = ConfigLoader(h)
		}
	}
	if env.Secrets == nil {
		env.Secrets = config.NewSecrets(env.Store.Home())
	}
	return &homeCredentials{PoolCredentials: NewPoolCredentials(env), env: env}
}

// True when load_pool for the provider holds rows, false when the pool cannot load
func (c *homeCredentials) HasPoolCredentials(provider string) bool {
	pool := c.Pool(provider)
	return pool != nil && pool.HasCredentials()
}

// Auth store login of an oauth provider through its runtime credential resolver, errors carried verbatim
func (c *homeCredentials) RuntimeCredential(provider string) (Credential, error) {
	var creds map[string]any
	var err error
	switch provider {
	case "nous":
		var seconds float64
		seconds, err = nousBillingFloat(c.env.scope().GetSecretStr("ICEBATH_NOUS_TIMEOUT_SECONDS", "15"))
		if err != nil {
			return Credential{}, err
		}
		creds, err = resolveNousRuntimeCredentialsWith(c.env, NousResolveOptions{Timeout: time.Duration(seconds * float64(time.Second))})
	case "openai-codex":
		creds, err = resolveCodexRuntimeCredentials(c.env, CodexResolveOptions{})
	case "xai-oauth":
		creds, err = resolveXAIOAuthRuntimeCredentials(c.env, false, true, 0)
	case "qwen-oauth":
		creds, err = resolveQwenRuntimeCredentials(c.env, true)
	case "minimax-oauth":
		creds, err = resolveMinimaxOAuthRuntimeCredentials(c.env)
	default:
		cred, ok := c.env.Store.Credential(provider)
		if !ok {
			return Credential{}, oauthMissingLoginError(provider)
		}
		return cred, nil
	}
	if err != nil {
		return Credential{}, err
	}
	return Credential{APIKey: anyString(creds["api_key"]), BaseURL: anyString(creds["base_url"]), Source: anyString(creds["source"])}, nil
}

// auth.json active provider when it is a registry provider reporting logged in, a guest without inference excluded
func (c *homeCredentials) ActiveLoggedInProvider(skipFreeTier bool) string {
	data, err := c.env.Store.Load()
	if err != nil {
		slog.Default().Debug("Could not pre-read active auth provider", "error", err)
		return ""
	}
	maybe, _ := data["active_provider"].(string)
	if maybe == "nous" && hasGuest(c.env) && (skipFreeTier || !profileFreeInferenceActive(c.env)) {
		return ""
	}
	if maybe == "" {
		return ""
	}
	if _, ok := registryProfileCtx(c.env.ctx(), maybe); !ok {
		return ""
	}
	if truthy(GetAuthStatus(c.env, maybe)["logged_in"]) {
		return maybe
	}
	return ""
}

// Welcome trial guest carrying inference on this install
func (c *homeCredentials) FreeInferenceActive() bool { return profileFreeInferenceActive(c.env) }

// Logged in and configured flags get_auth_status reports for the provider in the bound home
func providerAuthStatus(ctx context.Context, provider string) (loggedIn, configured bool) {
	status := GetAuthStatus(modelsPoolEnv(ctx), provider)
	return truthy(status["logged_in"]), truthy(status["configured"])
}
