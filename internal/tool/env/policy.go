package env

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	ForcePrefix          = "_ICEBATH_FORCE_"
	GatewayProfilePinEnv = "ICEBATH_GATEWAY_PROFILE_PIN"
)

var staticProviderBlocklist = []string{
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
	"ICEBATH_PORTAL_API_KEY", "QWEN_API_KEY",
	"ICEBATH_ANON_API_SECRET", "ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD",
	"ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", "ICEBATH_DASHBOARD_DRAIN_SECRET",
	"ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "ICEBATH_MEET_REALTIME_KEY",
}

var leftCoreSecretEnv = []string{"HASS_TOKEN", "ICEBATH_LANGFUSE_SECRET_KEY", "CAMOFOX_API_KEY"}

var leftCorePrivateEnv = []string{
	"HASS_URL", "ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_LANGFUSE_BASE_URL", "CAMOFOX_URL",
	"COPILOT_GITHUB_TOKEN", "COPILOT_API_BASE_URL", "COPILOT_ACP_BASE_URL",
}

var codeOnlyAdapterSecrets = []string{
	"MSGRAPH_CLIENT_SECRET", "MSGRAPH_WEBHOOK_CLIENT_STATE", "QQ_STT_API_KEY",
	"FEISHU_ENCRYPT_KEY", "FEISHU_VERIFICATION_TOKEN", "YUANBAO_APP_KEY",
}

var alwaysStripStatic = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_PATH",
	"GITHUB_APP_INSTALLATION_ID",
	"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN",
	"SLACK_SIGNING_SECRET", "GATEWAY_ALLOWED_USERS", "GATEWAY_ALLOW_ALL_USERS",
	"GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_DELIVERY_KEY",
	"EMAIL_PASSWORD", "ICEBATH_DASHBOARD_SESSION_TOKEN",
	"ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "ICEBATH_DASHBOARD_BASIC_AUTH_SECRET",
	"ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "ICEBATH_DASHBOARD_DRAIN_SECRET",
	"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "DAYTONA_API_KEY",
}

var supervisedIdentityVars = []string{
	"ICEBATH_LAUNCHD_LABEL", "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID",
}

var profileGateMarkers = []string{
	"_ALLOWED_", "_ALLOW_ALL_", "_ALLOW_FROM", "_ALLOW_BOTS", "_ALLOW_PUBLIC_", "_IGNORED_CHANNELS",
	"_NO_THREAD_CHANNELS", "_FREE_RESPONSE_CHANNELS", "_BACKFILL_CHANNELS", "_GROUP_ALLOWED",
}

type ProviderEnv interface {
	ProviderEnvNames() []string
}

type Platforms interface {
	CredentialEnv() []string
	RequiredEnv(ctx context.Context) ([]string, error)
	Names(ctx context.Context) []string
}

type SessionVar struct {
	Name  string
	Value string
	Bound bool
}

type SessionContext interface {
	Vars(ctx context.Context) ([]SessionVar, bool)
}

type PolicyOptions struct {
	Config    *config.Store
	Homes     *home.Resolver
	Providers ProviderEnv
	Platforms Platforms
	Session   SessionContext
	Backends  *Providers
}

type homeSecrets struct {
	stamp    string
	complete bool
	names    map[string]bool
}

type Policy struct {
	cfg       *config.Store
	homes     *home.Resolver
	platforms Platforms
	session   SessionContext
	backends  *Providers
	suffixes  []string
	blocklist map[string]bool
	adapter   map[string]bool
	strip     map[string]bool

	mu        sync.Mutex
	homeCache map[string]homeSecrets
}

func upperSet(names ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, list := range names {
		for _, n := range list {
			out[strings.ToUpper(n)] = true
		}
	}
	return out
}

func NewPolicy(o PolicyOptions) (*Policy, error) {
	switch {
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "env.PolicyOptions.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.PolicyOptions.Homes"}
	case o.Providers == nil:
		return nil, &tool.UnfilledError{Name: "env.ProviderEnv"}
	case o.Platforms == nil:
		return nil, &tool.UnfilledError{Name: "env.Platforms"}
	case o.Session == nil:
		return nil, &tool.UnfilledError{Name: "env.SessionContext"}
	case o.Backends == nil:
		return nil, &tool.UnfilledError{Name: "env.PolicyOptions.Backends"}
	}
	p := &Policy{cfg: o.Config, homes: o.Homes, platforms: o.Platforms, session: o.Session, backends: o.Backends,
		suffixes: config.PlatformSecretSuffixes(), homeCache: map[string]homeSecrets{}}
	reg := o.Config.EnvRegistry()
	provider := map[string]bool{}
	for _, list := range [][]string{staticProviderBlocklist, leftCoreSecretEnv, leftCorePrivateEnv, o.Providers.ProviderEnvNames()} {
		for _, n := range list {
			provider[n] = true
		}
	}
	for _, name := range reg.Names() {
		v, ok := reg.Get(name)
		if !ok {
			continue
		}
		password := v.Password != nil && *v.Password
		if v.Category == "tool" || v.Category == "messaging" || (v.Category == "setting" && password) {
			provider[name] = true
		}
	}
	delete(provider, "CLAUDE_CODE_OAUTH_TOKEN")
	adapter := map[string]bool{}
	for _, n := range codeOnlyAdapterSecrets {
		adapter[n] = true
	}
	for _, name := range reg.CoreDeclaredNames() {
		if v, ok := reg.Get(name); ok && v.Category == "messaging" && v.Password != nil && *v.Password {
			adapter[strings.ToUpper(name)] = true
		}
	}
	for _, n := range reg.BundledPlatformSecrets() {
		adapter[n] = true
	}
	for _, n := range o.Platforms.CredentialEnv() {
		if up := strings.ToUpper(n); p.secretSuffix(up) {
			adapter[up] = true
		}
	}
	p.adapter = adapter
	p.blocklist = map[string]bool{}
	for n := range provider {
		p.blocklist[n] = true
	}
	for n := range adapter {
		p.blocklist[n] = true
	}
	p.strip = upperSet(alwaysStripStatic, leftCoreSecretEnv)
	for n := range adapter {
		p.strip[strings.ToUpper(n)] = true
	}
	return p, nil
}

func (p *Policy) secretSuffix(upper string) bool {
	for _, s := range p.suffixes {
		if strings.HasSuffix(upper, s) {
			return true
		}
	}
	return false
}

func (p *Policy) homeAdapterSecrets(ctx context.Context) (map[string]bool, error) {
	homeDir, err := p.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	key, err := p.homes.Key(homeDir)
	if err != nil {
		return nil, err
	}
	env := p.cfg.Env()
	stamp, err := env.InstalledPlatformStamp(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	cached, ok := p.homeCache[key]
	p.mu.Unlock()
	if ok && cached.complete && cached.stamp == stamp {
		return cached.names, nil
	}
	list, complete, err := env.InstalledPlatformSecrets(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, n := range list {
		if !p.adapter[n] {
			names[n] = true
		}
	}
	if !complete && ok {
		for n := range cached.names {
			names[n] = true
		}
	}
	p.mu.Lock()
	p.homeCache[key] = homeSecrets{stamp: stamp, complete: complete, names: names}
	p.mu.Unlock()
	return names, nil
}

func (p *Policy) registryAdapterSecrets(ctx context.Context) (map[string]bool, error) {
	names, err := p.platforms.RequiredEnv(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, n := range names {
		if up := strings.ToUpper(n); p.secretSuffix(up) {
			out[up] = true
		}
	}
	return out, nil
}

func (p *Policy) RegisteredAdapterSecrets(ctx context.Context) (map[string]bool, error) {
	reg, err := p.registryAdapterSecrets(ctx)
	if err != nil {
		return nil, err
	}
	homeNames, err := p.homeAdapterSecrets(ctx)
	if err != nil {
		return nil, err
	}
	for n := range homeNames {
		reg[n] = true
	}
	return reg, nil
}

func (p *Policy) ProviderBlocklisted(ctx context.Context, name string, registered map[string]bool) (bool, error) {
	upper := strings.ToUpper(name)
	if p.blocklist[name] || p.blocklist[upper] {
		return true, nil
	}
	if registered == nil {
		var err error
		if registered, err = p.RegisteredAdapterSecrets(ctx); err != nil {
			return false, err
		}
	}
	return registered[upper], nil
}

func IsInternalSecret(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "AUXILIARY_") && (strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_BASE_URL")) {
		return true
	}
	return strings.HasPrefix(upper, "GATEWAY_RELAY_") &&
		(strings.HasSuffix(upper, "_SECRET") || strings.HasSuffix(upper, "_KEY") || strings.HasSuffix(upper, "_TOKEN"))
}

func FirstPartyPrefix(name string) bool {
	return strings.HasPrefix(strings.ToUpper(name), "BUZZ_")
}

func (p *Policy) sessionEnv(ctx context.Context, name, def string) string {
	vars, _ := p.session.Vars(ctx)
	for _, v := range vars {
		if v.Name == name && v.Bound {
			return v.Value
		}
	}
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func (p *Policy) buzzContextActive(ctx context.Context) bool {
	if os.Getenv("BUZZ_MANAGED_AGENT") != "" {
		return true
	}
	return strings.ToLower(strings.TrimSpace(p.sessionEnv(ctx, "ICEBATH_SESSION_PLATFORM", ""))) == "buzz"
}

func (p *Policy) terminalFirstParty(ctx context.Context, name string) bool {
	return FirstPartyPrefix(name) && p.buzzContextActive(ctx)
}

func CarryGatewayProfilePin(env map[string]string) map[string]string {
	switch strings.ToLower(strings.TrimSpace(env["ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR"])) {
	case "1", "true", "yes", "on":
		env[GatewayProfilePinEnv] = "1"
		return env
	}
	if env["ICEBATH_SUPERVISED_CHILD"] != "" {
		env[GatewayProfilePinEnv] = "1"
	}
	return env
}

func StripSupervisedIdentity(env map[string]string) map[string]string {
	CarryGatewayProfilePin(env)
	for _, name := range supervisedIdentityVars {
		delete(env, name)
	}
	return env
}

func (p *Policy) gatePrefixes(ctx context.Context) map[string]bool {
	out := map[string]bool{"GATEWAY": true, "QQ": true}
	for _, n := range p.platforms.Names(ctx) {
		if n != "" {
			out[strings.ReplaceAll(strings.ToUpper(n), "-", "_")] = true
		}
	}
	return out
}

func isProfileGateEnv(name string, prefixes map[string]bool) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "ICEBATH_") || strings.HasPrefix(upper, "_") {
		return false
	}
	marked := false
	for _, m := range profileGateMarkers {
		if strings.Contains(upper, m) {
			marked = true
			break
		}
	}
	if !marked {
		return false
	}
	for prefix := range prefixes {
		if strings.HasPrefix(upper, prefix+"_") {
			return true
		}
	}
	return false
}

func (p *Policy) IsProfileGateEnv(ctx context.Context, name string) bool {
	return isProfileGateEnv(name, p.gatePrefixes(ctx))
}

func (p *Policy) StripProfileGateEnv(ctx context.Context, env map[string]string) map[string]string {
	prefixes := p.gatePrefixes(ctx)
	for k := range env {
		if isProfileGateEnv(k, prefixes) {
			delete(env, k)
		}
	}
	return env
}

func (p *Policy) pluginStripKeys() map[string]bool {
	out := map[string]bool{}
	for _, k := range p.backends.StripEnvKeys() {
		out[k] = true
	}
	return out
}

func (p *Policy) alwaysStripFolded() map[string]bool {
	out := make(map[string]bool, len(p.strip))
	for k := range p.strip {
		out[k] = true
	}
	return out
}
