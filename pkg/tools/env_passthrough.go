package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Session scoped allowlist of env vars sandboxed children may receive

var awsSDKCredentialEnvVars = []string{"AWS_BEARER_TOKEN_BEDROCK"}

var staticProviderEnvBlocklist = []string{
	"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_API_BASE", "OPENAI_ORG_ID",
	"OPENAI_ORGANIZATION", "OPENROUTER_API_KEY", "ANTHROPIC_BASE_URL",
	"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "LLM_MODEL", "GOOGLE_API_KEY",
	"VERTEX_CREDENTIALS_PATH", "GOOGLE_APPLICATION_CREDENTIALS", "DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY", "GROQ_API_KEY", "TOGETHER_API_KEY", "PERPLEXITY_API_KEY",
	"COHERE_API_KEY", "FIREWORKS_API_KEY", "XAI_API_KEY", "HELICONE_API_KEY",
	"PARALLEL_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL",
	"TELEGRAM_HOME_CHANNEL", "TELEGRAM_HOME_CHANNEL_NAME", "DISCORD_HOME_CHANNEL",
	"DISCORD_HOME_CHANNEL_NAME", "DISCORD_REQUIRE_MENTION",
	"DISCORD_FREE_RESPONSE_CHANNELS", "DISCORD_AUTO_THREAD", "SLACK_HOME_CHANNEL",
	"SLACK_HOME_CHANNEL_NAME", "SLACK_ALLOWED_USERS", "WHATSAPP_ENABLED",
	"WHATSAPP_MODE", "WHATSAPP_ALLOWED_USERS", "SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT",
	"SIGNAL_ALLOWED_USERS", "SIGNAL_GROUP_ALLOWED_USERS", "SIGNAL_HOME_CHANNEL",
	"SIGNAL_HOME_CHANNEL_NAME", "SIGNAL_IGNORE_STORIES",
	"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST",
	"EMAIL_HOME_ADDRESS", "EMAIL_HOME_ADDRESS_NAME", "ICEBATH_DASHBOARD_SESSION_TOKEN",
	"GATEWAY_ALLOWED_USERS", "GH_TOKEN", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_PATH",
	"GITHUB_APP_INSTALLATION_ID", "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET",
	"DAYTONA_API_KEY", "GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET",
	"GATEWAY_RELAY_DELIVERY_KEY", "VERCEL_OIDC_TOKEN", "VERCEL_TOKEN",
	"VERCEL_PROJECT_ID", "VERCEL_TEAM_ID",
	"QWEN_API_KEY", "ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD",
	"ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", "ICEBATH_DASHBOARD_DRAIN_SECRET",
	"ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "ICEBATH_MEET_REALTIME_KEY",
}

// Env of features that left core for a catalog plugin, stripped exactly as when core shipped them
var leftCoreEnv = []string{
	"HASS_TOKEN", "HASS_URL",
	"ICEBATH_LANGFUSE_SECRET_KEY", "ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_LANGFUSE_BASE_URL",
	"CAMOFOX_API_KEY", "CAMOFOX_URL",
	"COPILOT_GITHUB_TOKEN", "COPILOT_API_BASE_URL", "COPILOT_ACP_BASE_URL",
}

// Secrets read in code only that no manifest declares
var staticAdapterSecretEnv = []string{
	"MSGRAPH_CLIENT_SECRET", "MSGRAPH_WEBHOOK_CLIENT_STATE", "QQ_STT_API_KEY",
	"FEISHU_ENCRYPT_KEY", "FEISHU_VERIFICATION_TOKEN", "YUANBAO_APP_KEY",
}

// PlatformSecretEnvSuffixes mark gateway env override keys that hold secrets
var PlatformSecretEnvSuffixes = []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_JSON"}

// BundledPlatformSecretEnvs lists the bundled platform plugin manifests' secret names, installed by this package's init
var BundledPlatformSecretEnvs func() []string

// ConfigEnvTableKeys lists the gateway env override table keys, installed by the gateway package
var ConfigEnvTableKeys func() []string

// RegisteredAdapterSecretEnv lists secrets declared by registered and profile installed platform plugins
var RegisteredAdapterSecretEnv func(env *Env) map[string]bool

// Refuses to build a blocklist before the gateway env table and platform registry sources are wired
func requireAdapterSecretSources() {
	if ConfigEnvTableKeys == nil || RegisteredAdapterSecretEnv == nil {
		panic("tools: the gateway must install ConfigEnvTableKeys (profile channels env table) and RegisteredAdapterSecretEnv (platform registry) before the provider env blocklist is read")
	}
}

// Derives the blocklist from the static list, the provider registry, and tool or messaging secrets
func buildProviderEnvBlocklist() map[string]bool {
	blocked := map[string]bool{}
	for _, n := range staticProviderEnvBlocklist {
		blocked[n] = true
	}
	for _, n := range leftCoreEnv {
		blocked[n] = true
	}
	for _, n := range awsSDKCredentialEnvVars {
		blocked[n] = true
	}
	for _, item := range config.OptionalEnvVars.Items() {
		name, meta := item.Name, item.Entry
		switch {
		case meta.Category == "provider":
			blocked[name] = true
		case meta.Category == "tool" || meta.Category == "messaging":
			blocked[name] = true
		case meta.Category == "setting" && meta.Password:
			blocked[name] = true
		}
	}
	delete(blocked, "CLAUDE_CODE_OAUTH_TOKEN")
	return blocked
}

// Adapter secrets declared process wide by core messaging entries, bundled manifests, and the env table
func buildAdapterSecretEnv() map[string]bool {
	requireAdapterSecretSources()
	names := map[string]bool{}
	for _, n := range staticAdapterSecretEnv {
		names[n] = true
	}
	core, err := config.CoreDeclaredEnvNames()
	if err != nil {
		panic(fmt.Sprintf("tools: core declared env names: %v", err))
	}
	for _, item := range config.OptionalEnvVars.Items() {
		name, meta := item.Name, item.Entry
		if core[name] && meta.Category == "messaging" && meta.Password {
			names[strings.ToUpper(name)] = true
		}
	}
	for _, n := range BundledPlatformSecretEnvs() {
		names[n] = true
	}
	for _, n := range ConfigEnvTableKeys() {
		upper := strings.ToUpper(n)
		for _, suffix := range PlatformSecretEnvSuffixes {
			if strings.HasSuffix(upper, suffix) {
				names[upper] = true
				break
			}
		}
	}
	return names
}

var (
	providerBlocklistMu sync.Mutex
	providerBlocklist   map[string]bool
)

// ProviderEnvBlocklist is the union of provider credentials and adapter secrets, built on first use
func ProviderEnvBlocklist() map[string]bool {
	providerBlocklistMu.Lock()
	defer providerBlocklistMu.Unlock()
	if providerBlocklist == nil {
		providerBlocklist = buildProviderEnvBlocklist()
		for n := range buildAdapterSecretEnv() {
			providerBlocklist[n] = true
		}
	}
	return providerBlocklist
}

// IsIcebathInternalSecret is true for dynamically named internal secrets the static list cannot enumerate
func IsIcebathInternalSecret(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "AUXILIARY_") && (strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_BASE_URL")) {
		return true
	}
	return strings.HasPrefix(upper, "GATEWAY_RELAY_") && (strings.HasSuffix(upper, "_SECRET") || strings.HasSuffix(upper, "_KEY") || strings.HasSuffix(upper, "_TOKEN"))
}

// IsProviderEnvBlocklisted matches exact and case folded names against the blocklist and registered adapters
func IsProviderEnvBlocklisted(env *Env, name string) bool {
	upper := strings.ToUpper(name)
	list := ProviderEnvBlocklist()
	if list[name] || list[upper] {
		return true
	}
	requireAdapterSecretSources()
	return RegisteredAdapterSecretEnv(env)[upper]
}

// True when the name is an icebath managed provider credential a skill must not override
func isIcebathProviderCredential(env *Env, name string) bool {
	return IsIcebathInternalSecret(name) || IsProviderEnvBlocklisted(env, name)
}

var (
	passthroughMu      sync.Mutex
	passthroughAllowed = map[string]bool{}
	passthroughConfig  = map[string]map[string]bool{}
)

// Yields non empty names that are not provider credentials, logging each refusal
func passthroughAccepted(env *Env, names []string, refusal string) []string {
	var out []string
	for _, name := range names {
		if name == "" {
			continue
		}
		if isIcebathProviderCredential(env, name) {
			envLog(env).Warn(refusal, "name", name)
			continue
		}
		out = append(out, name)
	}
	return out
}

// RegisterEnvPassthrough allows env var names in sandboxed environments, refusing provider credentials
func RegisterEnvPassthrough(env *Env, varNames []string) {
	trimmed := make([]string, 0, len(varNames))
	for _, n := range varNames {
		trimmed = append(trimmed, strings.TrimSpace(n))
	}
	for _, name := range passthroughAccepted(env, trimmed,
		"env passthrough: refusing to register icebath provider credential (blocked by the provider env blocklist). "+
			"Skills must not override the execute_code sandbox's credential scrubbing; see GHSA-rhgp-j443-p4rf.") {
		passthroughMu.Lock()
		passthroughAllowed[name] = true
		passthroughMu.Unlock()
		pkgLog().Debug("env passthrough: registered", "name", name)
	}
}

// Loads terminal.env_passthrough from config.yaml cached per home with the same credential filter
func loadConfigPassthrough(env *Env) map[string]bool {
	homeKey := ""
	if env != nil && env.Home != "" {
		homeKey = home.Key(env.Home)
	}
	passthroughMu.Lock()
	cached, ok := passthroughConfig[homeKey]
	passthroughMu.Unlock()
	if ok {
		return cached
	}
	result := map[string]bool{}
	var items []string
	if terminal := cfgTree(env).Map("terminal"); terminal != nil {
		if list, ok := terminal["env_passthrough"].([]any); ok {
			for _, i := range list {
				if s, ok := i.(string); ok {
					items = append(items, strings.TrimSpace(s))
				}
			}
		}
	}
	for _, name := range passthroughAccepted(env, items,
		"env passthrough: refusing to register icebath provider credential from config.yaml (blocked by the provider env blocklist). "+
			"Operator configuration must not override the execute_code sandbox's credential scrubbing; see GHSA-rhgp-j443-p4rf.") {
		result[name] = true
	}
	passthroughMu.Lock()
	passthroughConfig[homeKey] = result
	passthroughMu.Unlock()
	return result
}

// IsEnvPassthrough is true for a registered or configured name that is not a managed credential now
func IsEnvPassthrough(env *Env, varName string) bool {
	passthroughMu.Lock()
	registered := passthroughAllowed[varName]
	passthroughMu.Unlock()
	return (registered || loadConfigPassthrough(env)[varName]) && !isIcebathProviderCredential(env, varName)
}

// AllPassthrough returns the union of skill registered and configured names minus managed credentials
func AllPassthrough(env *Env) map[string]bool {
	out := map[string]bool{}
	passthroughMu.Lock()
	for n := range passthroughAllowed {
		out[n] = true
	}
	passthroughMu.Unlock()
	for n := range loadConfigPassthrough(env) {
		out[n] = true
	}
	for n := range out {
		if isIcebathProviderCredential(env, n) {
			delete(out, n)
		}
	}
	return out
}

var globalEnvExact = map[string]bool{
	"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_GATEWAY_LOCK_DIR": true,
	"ICEBATH_MAX_ITERATIONS": true, "ICEBATH_API_TIMEOUT": true,
	"ICEBATH_REDACT_SECRETS": true,
	"_ICEBATH_GATEWAY":       true,
	"PATH":                   true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true, "TZ": true, "PWD": true, "SHELL": true, "TMPDIR": true,
	"VIRTUAL_ENV": true, "PYTHONPATH": true, "SSL_CERT_FILE": true,
}

var globalEnvPrefixes = []string{"ICEBATH_KANBAN_", "ICEBATH_TELEGRAM_", "TERMINAL_"}

// IsGlobalEnv is true for process wide settings that are never profile secrets
func IsGlobalEnv(name string) bool {
	if globalEnvExact[name] {
		return true
	}
	for _, p := range globalEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ErrUnscopedSecret is returned when a multiplexing process reads a secret without a bound profile scope
var ErrUnscopedSecret = errors.New("secret read without a bound profile scope while profile multiplexing is active")

// Process wide multiplex flag a gateway raises when it serves many profiles, the set_multiplex_active state
var multiplexActiveFlag atomic.Bool

// SetMultiplexActive marks this process as a profile multiplexer so unscoped secret reads fail closed
func SetMultiplexActive(active bool) { multiplexActiveFlag.Store(active) }

// MultiplexActive reports the process wide multiplex flag
func MultiplexActive() bool { return multiplexActiveFlag.Load() }

// Whether this process serves many profiles so an unscoped secret read must fail closed
func isMultiplexActive(env *Env) bool {
	return multiplexActiveFlag.Load() || sessionFlag(env, "multiplex", "ICEBATH_MULTIPLEX_ACTIVE")
}

// Whether one call runs under multiplex semantics, from the process flag, the tool env or the context
func MultiplexActiveFor(ctx context.Context, env *Env) bool {
	return isMultiplexActive(env) || (config.IsMultiplexActive != nil && config.IsMultiplexActive(ctx))
}

// Reader of the secret mapping a context binds, nil when none, installed by pkg/provider
var BoundSecretScope func(ctx context.Context) map[string]string

// Home the context's bound secret scope was built for, installed by pkg/provider
var BoundSecretScopeHome func(ctx context.Context) string

// Launch mapping of a body entered before multiplexing began, nil outside one, installed by pkg/provider
var LaunchBodySecretScope func(ctx context.Context) map[string]string

// Mapping the context binds, read through the installed secret source and else through pkg/provider's own reader
func contextSecretScope(ctx context.Context) map[string]string {
	if ProfileSecrets != nil {
		return ProfileSecrets.CurrentSecretScope(ctx)
	}
	if BoundSecretScope != nil {
		return BoundSecretScope(ctx)
	}
	return nil
}

// Mapping a secret read resolves against with its home, the context's bound scope before the tool env's secrets
func passthroughScope(ctx context.Context, env *Env) (map[string]string, string, bool) {
	if scope := contextSecretScope(ctx); scope != nil {
		scopeHome := ""
		if BoundSecretScopeHome != nil {
			scopeHome = BoundSecretScopeHome(ctx)
		}
		return scope, scopeHome, true
	}
	if env != nil && env.Secrets != nil {
		return env.Secrets.File(), filepath.Dir(env.Secrets.Path()), true
	}
	return nil, "", false
}

// Whether a scoped miss returns the default rather than the process env, the serves_routed_profile verdict of a call
func passthroughServesRouted(ctx context.Context, env *Env, scopeHome string) bool {
	if MultiplexActiveFor(ctx, env) {
		return true
	}
	own := home.Key(home.RoutingProcess())
	if scopeHome != "" && home.Key(scopeHome) != own {
		return true
	}
	override := home.Override(ctx)
	return override != "" && home.Key(override) != own
}

// Value of an allowlisted variable resolved through the bound profile secret scope
func ResolvePassthroughValue(ctx context.Context, env *Env, name string, fallback *string) (*string, error) {
	global := IsGlobalEnv(name)
	if global && fallback != nil {
		return fallback, nil
	}
	multiplex := MultiplexActiveFor(ctx, env)
	scope, scopeHome, bound := passthroughScope(ctx, env)
	if !bound {
		if !multiplex {
			return fallback, nil
		}
		return unscopedMultiplexSecret(ctx, name)
	}
	def := fallback
	if multiplex {
		def = nil
	}
	if global {
		return processEnvOr(name, def), nil
	}
	if value, ok := scope[name]; ok {
		return &value, nil
	}
	if passthroughServesRouted(ctx, env, scopeHome) {
		return def, nil
	}
	return processEnvOr(name, def), nil
}

// get_secret of one name through the call's bound scope, the tool env's secrets standing in when the context binds none
func secretThroughScope(ctx context.Context, env *Env, name string) (*string, error) {
	if IsGlobalEnv(name) {
		return processEnvValue(name), nil
	}
	if scope := contextSecretScope(ctx); scope != nil {
		if value, ok := scope[name]; ok {
			return &value, nil
		}
		scopeHome := ""
		if BoundSecretScopeHome != nil {
			scopeHome = BoundSecretScopeHome(ctx)
		}
		if passthroughServesRouted(ctx, env, scopeHome) {
			return nil, nil
		}
		return processEnvValue(name), nil
	}
	if env != nil && env.Secrets != nil {
		return envSecretsValue(ctx, env, name), nil
	}
	if MultiplexActiveFor(ctx, env) {
		return unscopedMultiplexSecret(ctx, name)
	}
	return processEnvValue(name), nil
}

// Value from the tool env's own secrets, a launch profile's mapping holding the process env under its env file
func envSecretsValue(ctx context.Context, env *Env, name string) *string {
	if value, ok := env.Secrets.File()[name]; ok {
		return &value
	}
	own := home.Key(home.RoutingProcess())
	if home.Key(filepath.Dir(env.Secrets.Path())) != own {
		return nil
	}
	if override := home.Override(ctx); override != "" && home.Key(override) != own {
		return nil
	}
	return processEnvValue(name)
}

// Unscoped read under multiplexing, the launch body mapping when one is bound and the fail closed error otherwise
func unscopedMultiplexSecret(ctx context.Context, name string) (*string, error) {
	if IsGlobalEnv(name) {
		return processEnvValue(name), nil
	}
	if LaunchBodySecretScope != nil {
		if body := LaunchBodySecretScope(ctx); body != nil {
			if value, ok := body[name]; ok {
				return &value, nil
			}
			return nil, nil
		}
	}
	return nil, ErrUnscopedSecret
}

// Reports whether the bound secret scope belongs to a profile other than the routing launch home
func servesRoutedEnv(env *Env) bool {
	if isMultiplexActive(env) {
		return true
	}
	scopeHome := ""
	if env != nil && env.Secrets != nil {
		scopeHome = filepath.Dir(env.Secrets.Path())
	} else if env != nil {
		scopeHome = env.Home
	}
	return scopeHome != "" && home.Key(scopeHome) != home.Key(home.RoutingProcess())
}

// Declared names the bound profile scope supplies but the filtered env lacks
func ScopedPassthroughAdditions(ctx context.Context, env *Env, present map[string]bool) map[string]string {
	additions := map[string]string{}
	names := AllPassthrough(env)
	if len(names) == 0 {
		return additions
	}
	scope, _, bound := passthroughScope(ctx, env)
	if !bound || len(scope) == 0 {
		return additions
	}
	for name := range names {
		if present[name] || IsGlobalEnv(name) {
			continue
		}
		if value, ok := scope[name]; ok {
			additions[name] = value
		}
	}
	return additions
}

// ClearEnvPassthrough resets the skill registered allowlist on session reset
func ClearEnvPassthrough() {
	passthroughMu.Lock()
	passthroughAllowed = map[string]bool{}
	passthroughMu.Unlock()
}

// ResetEnvPassthroughConfigCache drops the per home config allowlist cache
func ResetEnvPassthroughConfigCache() {
	passthroughMu.Lock()
	passthroughConfig = map[string]map[string]bool{}
	passthroughMu.Unlock()
	providerBlocklistMu.Lock()
	providerBlocklist = nil
	providerBlocklistMu.Unlock()
}

// Reads a process env value the way os.getenv does for callers that need the raw fallback
func processEnvValue(name string) *string {
	v, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	return &v
}

// Process env value of the name, the default when it is unset
func processEnvOr(name string, def *string) *string {
	if value := processEnvValue(name); value != nil {
		return value
	}
	return def
}
