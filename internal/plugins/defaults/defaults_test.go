package defaults

import (
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/client"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
	providerserver "github.com/nickheyer/icebath/internal/provider/server"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
)

type hosts struct {
	config      *config.Config
	server      *server.Server
	app         *app.App
	cli         *cli.CLI
	provider    *provider.Provider
	transport   *transport.Transport
	credentials *credentials.Credentials
	models      *models.Models
	tools       *tools.Tools
	sessions    *sessions.Sessions
	slash       *slash.Slash
}

func register(t *testing.T) *hosts {
	t.Helper()
	cfg := config.New(t.TempDir())
	h := &hosts{
		config: cfg, server: server.New(cfg), app: app.New(), cli: cli.New(), provider: provider.New(), transport: transport.New(),
		credentials: credentials.New(), models: models.New(), tools: tools.New(cfg), sessions: sessions.New(cfg), slash: slash.New(),
	}
	providerserver.Register(h.server, h.provider, h.credentials, h.models)
	providerapp.Register(h.app)
	providercli.Register(h.cli)
	Register(plugins.New(cfg, h.server, h.app, h.cli, h.provider, h.transport, h.credentials, h.models, h.tools, h.sessions, h.slash))
	t.Cleanup(func() {
		h.server.Close()
		h.sessions.Close()
	})
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	variables := []string{"AWS_BEARER_TOKEN_BEDROCK"}
	for _, profile := range h.provider.Profiles() {
		variables = append(append(variables, profile.KeyEnv...), profile.BaseURLEnv)
	}
	for _, row := range alphaRows {
		variables = append(append(variables, row.keys...), row.baseEnv)
	}
	for _, variable := range variables {
		if variable != "" {
			t.Setenv(variable, "")
		}
	}
	return h
}

func TestEveryAlphaRegistryRowIsAProviderPlugin(t *testing.T) {
	h := register(t)
	for _, row := range alphaRows {
		profile, ok := h.provider.Profile(row.name)
		if !ok || profile.Name != row.name {
			t.Errorf("%s: no provider plugin registers it", row.name)
			continue
		}
		want := row.keys
		if profile.DisplayName != row.display || profile.BaseURL != row.base || profile.BaseURLEnv != row.baseEnv ||
			string(profile.Mode) != row.mode || profile.AuthType != row.authType || profile.Auth != provider.AuthKindOf(row.authType) || !slices.Equal(profile.KeyEnv, want) {
			t.Errorf("%s: profile = {%q %q %q %q %q %q %v}\n\twant alpha's {%q %q %q %q %q %q %v}", row.name,
				profile.DisplayName, profile.BaseURL, profile.BaseURLEnv, profile.Mode, profile.AuthType, profile.Auth, profile.KeyEnv,
				row.display, row.base, row.baseEnv, row.mode, row.authType, provider.AuthKindOf(row.authType), want)
		}
		if profile.DefaultModel != row.defModel {
			t.Errorf("%s: default model = %q, want alpha's %q", row.name, profile.DefaultModel, row.defModel)
		}
	}
	for name, aliases := range alphaAliases {
		if _, registered := h.provider.Profile(name); !registered {
			if name != "custom" {
				t.Errorf("%s: alpha has aliases for it and no provider plugin registers it", name)
			}
			continue
		}
		for _, alias := range aliases {
			if profile, ok := h.provider.Profile(alias); !ok || profile.Name != name {
				t.Errorf("alias %s resolves to %q, want %s", alias, profile.Name, name)
			}
		}
	}
	seen := map[string]string{}
	for _, profile := range h.provider.Profiles() {
		for _, name := range append([]string{profile.Name}, profile.Aliases...) {
			if owner, taken := seen[name]; taken && owner != profile.Name && alphaAliases[owner] == nil {
				t.Errorf("%s is claimed by both %s and %s", name, owner, profile.Name)
			}
			seen[name] = profile.Name
		}
		if profile.ModelListing && profile.ModelsURL == "" && profile.BaseURL != "" && profile.Name != "tencent-tokenhub" {
			t.Errorf("%s lists models and has no models url", profile.Name)
		}
	}
}

func TestProviderModesAndTheWiresThatExist(t *testing.T) {
	h := register(t)
	missing := map[string]bool{}
	for _, profile := range h.provider.Profiles() {
		if _, ok := h.transport.Factories()[profile.Mode]; !ok {
			missing[string(profile.Mode)] = true
		}
	}
	var modes []string
	for mode := range missing {
		modes = append(modes, mode)
	}
	slices.Sort(modes)
	if got, want := strings.Join(modes, " "), "bedrock_converse codex_responses"; got != want {
		t.Fatalf("modes without a registered wire = %q, want %q: update this when a wire lands or a profile needs a new one", got, want)
	}
}

func TestEveryDefaultPluginRegistersItself(t *testing.T) {
	h := register(t)
	cfg, p, cr, tl, sl, s := h.config, h.provider, h.credentials, h.tools, h.slash, h.sessions
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	t.Setenv("ICEBATH_TEST_LOCAL_KEY", "local-key")
	if err := cfg.Set("plugins.custom.providers.local", map[string]any{"base_url": "http://127.0.0.1:1/v1", "key_env": "ICEBATH_TEST_LOCAL_KEY", "default_model": "tiny"}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	for alias, want := range map[string]string{"claude": "anthropic", "or": "openrouter", "openai": "openrouter", "LOCAL": "local", "grok": "xai", "github": "copilot"} {
		if profile, ok := p.Profile(alias); !ok || profile.Name != want {
			t.Errorf("Profile(%q) = %q, %v, want %s", alias, profile.Name, ok, want)
		}
	}
	if local, _ := p.Profile("local"); local.Mode != provider.ModeChatCompletions || !local.Local || local.DefaultModel != "tiny" {
		t.Errorf("custom profile = %+v", local)
	}

	if _, ok := cr.Credential("anthropic"); ok {
		t.Error("anthropic has a credential with none set")
	}
	if credential, ok := cr.Credential("openrouter"); !ok || credential.APIKey != "or-key" || credential.Source != "OPENROUTER_API_KEY" {
		t.Errorf("openrouter credential = %+v, %v", credential, ok)
	}
	if credential, ok := cr.Credential("local"); !ok || credential.APIKey != "local-key" || credential.Source != "ICEBATH_TEST_LOCAL_KEY" {
		t.Errorf("local credential = %+v, %v", credential, ok)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-key")
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-token")
	if credential, ok := cr.Credential("anthropic"); !ok || credential.APIKey != "sk-ant-oat01-token" || credential.Auth != provider.AuthOAuth || credential.Source != "ANTHROPIC_TOKEN" {
		t.Errorf("anthropic credential = %+v, %v, want the oauth token variable before the key variable", credential, ok)
	}
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://proxy.test")
	anthropic, _ := p.Profile("anthropic")
	if route, err := cr.Route(anthropic, ""); err != nil || route.APIKey != "sk-key" || route.Auth != provider.AuthAPIKey || route.BaseURL != "https://proxy.test" || route.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("anthropic route = %+v, %v, want the api key and the endpoint from the environment", route, err)
	}
	t.Setenv("OPENCODE_ZEN_API_KEY", "zen-key")
	zen, _ := p.Profile("zen")
	for model, want := range map[string]provider.APIMode{"claude-sonnet-5": provider.ModeAnthropicMessages, "gpt-5.5": provider.ModeCodexResponses, "glm-5": provider.ModeChatCompletions} {
		if route, err := cr.Route(zen, model); err != nil || route.Mode != want {
			t.Errorf("opencode-zen route for %s = %q, %v, want the wire of that model family %q", model, route.Mode, err, want)
		}
	}

	var offered []string
	for _, def := range tl.Tools() {
		offered = append(offered, def.Name)
	}
	slices.Sort(offered)
	if got, want := strings.Join(offered, " "), "patch read_file search_files terminal write_file"; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
	var commands []string
	for _, command := range sl.Commands() {
		commands = append(commands, command.Name)
	}
	if got, want := strings.Join(commands, " "), "help title retry undo approve deny yolo model"; got != want {
		t.Errorf("slash commands = %s, want %s", got, want)
	}
	if _, err := s.Store(cfg.GetString(sessionsconfig.Store)); err != nil {
		t.Errorf("the configured session store is not registered: %v", err)
	}

	conn := client.InProcess(h.server.Services())
	listed, err := providerv1connect.NewProviderServiceClient(conn).List(t.Context(), &providerv1.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	rows := map[string]string{}
	for _, entry := range listed.GetProviders() {
		rows[entry.GetName()] = entry.GetDisplayName() + "|" + entry.GetCredentialSource() + "|" + entry.GetBaseUrl() + "|" + strings.Join(entry.GetKeyEnv(), ",")
	}
	for name, want := range map[string]string{
		"anthropic":  "Anthropic|ANTHROPIC_API_KEY|https://proxy.test|ANTHROPIC_API_KEY,ANTHROPIC_TOKEN,CLAUDE_CODE_OAUTH_TOKEN",
		"openrouter": "OpenRouter|OPENROUTER_API_KEY|https://openrouter.ai/api/v1|OPENROUTER_API_KEY",
		"local":      "local|ICEBATH_TEST_LOCAL_KEY|http://127.0.0.1:1/v1|",
		"deepseek":   "DeepSeek||https://api.deepseek.com/v1|DEEPSEEK_API_KEY",
	} {
		if rows[name] != want {
			t.Errorf("listed %s = %q, want %q", name, rows[name], want)
		}
	}
	if len(rows) != len(p.Profiles()) {
		t.Errorf("listed %d providers of %d registered", len(rows), len(p.Profiles()))
	}
	if _, err := providerv1connect.NewProviderServiceClient(conn).Models(t.Context(), &providerv1.ModelsRequest{Provider: "nowhere"}); err == nil || !strings.Contains(err.Error(), "unknown provider nowhere") {
		t.Errorf("Models() of an unknown provider error = %v", err)
	}

	var titles []string
	for _, screen := range h.app.Screens(conn) {
		titles = append(titles, screen.Title)
	}
	if got, want := strings.Join(titles, " "), "providers apluginexample chat sessions"; got != want {
		t.Errorf("screens = %s, want %s", got, want)
	}
	var uses []string
	for _, command := range h.cli.Commands(conn) {
		uses = append(uses, command.Name())
	}
	if got, want := strings.Join(uses, " "), "providers models apluginexample"; got != want {
		t.Errorf("cli commands = %s, want %s", got, want)
	}
}
