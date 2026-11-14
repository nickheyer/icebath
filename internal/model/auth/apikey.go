package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/shell"
)

var providerAliases = map[string]string{
	"glm": "zai", "z-ai": "zai", "z.ai": "zai", "zhipu": "zai",
	"google": "gemini", "google-gemini": "gemini", "google-ai-studio": "gemini",
	"x-ai": "xai", "x.ai": "xai", "grok": "xai",
	"xai-oauth": "xai-oauth", "x-ai-oauth": "xai-oauth",
	"grok-oauth": "xai-oauth", "xai-grok-oauth": "xai-oauth",
	"kimi": "kimi-coding", "kimi-for-coding": "kimi-coding", "moonshot": "kimi-coding",
	"kimi-cn": "kimi-coding-cn", "moonshot-cn": "kimi-coding-cn",
	"step": "stepfun", "stepfun-coding-plan": "stepfun",
	"arcee-ai": "arcee", "arceeai": "arcee",
	"gmi-cloud": "gmi", "gmicloud": "gmi",
	"actual-computer": "actual", "actualcomputer": "actual", "aci": "actual",
	"minimax-china": "minimax-cn", "minimax_cn": "minimax-cn",
	"minimax-portal": "minimax-oauth", "minimax-global": "minimax-oauth", "minimax_oauth": "minimax-oauth",
	"alibaba_coding": "alibaba-coding-plan", "alibaba-coding": "alibaba-coding-plan",
	"alibaba_coding_plan": "alibaba-coding-plan",
	"claude":              "anthropic", "claude-code": "anthropic",
	"aigateway": "ai-gateway", "vercel": "ai-gateway", "vercel-ai-gateway": "ai-gateway",
	"opencode": "opencode-zen", "zen": "opencode-zen",
	"qwen-portal": "qwen-oauth", "qwen-cli": "qwen-oauth", "qwen-oauth": "qwen-oauth",
	"hf": "huggingface", "hugging-face": "huggingface", "huggingface-hub": "huggingface",
	"mimo": "xiaomi", "xiaomi-mimo": "xiaomi",
	"tencent": "tencent-tokenhub", "tokenhub": "tencent-tokenhub",
	"tencent-cloud": "tencent-tokenhub", "tencentmaas": "tencent-tokenhub",
	"tokenplan": "tencent-tokenplan", "tencent-lkeap": "tencent-tokenplan",
	"aws": "bedrock", "aws-bedrock": "bedrock", "amazon-bedrock": "bedrock", "amazon": "bedrock",
	"go": "opencode-go", "opencode-go-sub": "opencode-go",
	"kilo": "kilocode", "kilo-code": "kilocode", "kilo-gateway": "kilocode",
	"lmstudio": "lmstudio", "lm-studio": "lmstudio", "lm_studio": "lmstudio",
	"chatgpt": "openai-codex", "chatgpt-codex": "openai-codex",
	"local":  "custom",
	"ollama": "custom", "ollama_cloud": "ollama-cloud",
	"vllm": "custom", "llamacpp": "custom",
	"llama.cpp": "custom", "llama-cpp": "custom",
}

func ProviderAlias(name string) (string, bool) {
	target, ok := providerAliases[strings.ToLower(strings.TrimSpace(name))]
	return target, ok
}

func canonicalProvider(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if target, ok := providerAliases[normalized]; ok {
		return target
	}
	return normalized
}

func IsActualLocalBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
	return slices.Contains([]string{"localhost", "127.0.0.1", "::1", "0.0.0.0"}, host)
}

func NormalizeActualBaseURL(baseURL string) string {
	target := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if target == "" {
		return DefaultActualBaseURL
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	host := strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" && (host == "api.actual.inc" || IsActualLocalBaseURL(target)) {
		return target + "/v1"
	}
	return target
}

func (s *Store) AnthropicKey(ctx context.Context) (Secret, error) {
	ctx = s.bind(ctx)
	spec, _ := s.dir.Spec(ctx, ProviderAnthropic)
	for _, name := range spec.EnvVars {
		v, err := s.preferFile(ctx, name)
		if err != nil {
			return Secret{}, err
		}
		if v != "" {
			return NewSecret(v), nil
		}
	}
	return Secret{}, nil
}

func (s *Store) modelLevelKeyEnv(ctx context.Context, provider string) string {
	cfg, err := s.cfg.LoadReadonly(ctx)
	if err != nil || cfg.Model.Bare {
		return ""
	}
	if strings.ToLower(strings.TrimSpace(cfg.Model.Provider)) != provider {
		return ""
	}
	if env := strings.TrimSpace(cfg.Model.KeyEnv); env != "" {
		return env
	}
	return strings.TrimSpace(cfg.Model.APIKeyEnv)
}

func (s *Store) resolveAPIKeySecret(ctx context.Context, spec SeedSpec) (key, source string, err error) {
	if spec.ResolveSecret != nil {
		if owned, from, ok := spec.ResolveSecret(ctx); ok {
			return owned.Reveal(), from, nil
		}
	}
	names := []string{}
	if keyEnv := s.modelLevelKeyEnv(ctx, spec.Provider); keyEnv != "" {
		names = append(names, keyEnv)
	}
	for _, name := range append(names, spec.EnvVars...) {
		raw, err := s.preferFile(ctx, name)
		if err != nil {
			return "", "", err
		}
		if val, ok := usableDeclaredSecret(spec.Provider, raw, name); ok {
			return val, name, nil
		}
	}
	poolSource := "credential_pool:" + spec.Provider
	pool, perr := s.Pool(ctx, spec.Provider)
	if perr != nil || !pool.HasCredentials() {
		return "", "", nil
	}
	candidates := []Entry{}
	first, ok, err := pool.Peek(ctx)
	if err != nil {
		return "", "", nil
	}
	if ok {
		candidates = append(candidates, first)
	}
	for _, extra := range pool.Entries() {
		if !slices.ContainsFunc(candidates, func(c Entry) bool { return c.ID == extra.ID }) {
			candidates = append(candidates, extra)
		}
	}
	for i := range candidates {
		token := candidates[i].AccessToken.Reveal()
		if token == "" {
			token = candidates[i].RuntimeAPIKey().Reveal()
		}
		if val, ok := usableDeclaredSecret(spec.Provider, token, poolSource); ok {
			return val, poolSource, nil
		}
	}
	return "", "", nil
}

func (s *Store) providerEnvBaseURL(ctx context.Context, spec SeedSpec) string {
	if spec.Provider == "actual" {
		if cfg, err := s.cfg.LoadReadonly(ctx); err == nil && !cfg.Model.Bare && canonicalProvider(cfg.Model.Provider) == "actual" {
			if configured := strings.TrimSpace(cfg.Model.BaseURL); configured != "" {
				return configured
			}
		}
	}
	if spec.BaseURLEnv == "" {
		return ""
	}
	return strings.TrimSpace(s.lookupEnv(ctx, spec.BaseURLEnv))
}

func (s *Store) APIKeyStatus(ctx context.Context, provider string) (Status, error) {
	ctx = s.bind(ctx)
	spec, ok := s.dir.Spec(ctx, provider)
	if !ok || spec.AuthType != AuthTypeAPIKey {
		return Status{}, nil
	}
	key, source, err := s.resolveAPIKeySecret(ctx, spec)
	if err != nil {
		return Status{}, err
	}
	envURL := s.providerEnvBaseURL(ctx, spec)
	baseURL := envURL
	if baseURL == "" {
		baseURL = spec.BaseURL
	}
	if (provider == "kimi-coding" || provider == "kimi-coding-cn") && spec.RuntimeBaseURL != nil {
		if owned, ok := spec.RuntimeBaseURL(ctx, NewSecret(key), spec.BaseURL, envURL); ok {
			baseURL = owned
		}
	}
	localNoAuth := false
	if provider == "actual" {
		baseURL = NormalizeActualBaseURL(baseURL)
		localNoAuth = key == "" && IsActualLocalBaseURL(baseURL)
	}
	configured := key != "" || localNoAuth
	if source == "" && localNoAuth {
		source = "local-offline"
	}
	return Status{Configured: configured, Provider: provider, Name: spec.Name, KeySource: source, BaseURL: baseURL, LoggedIn: configured}, nil
}

type APIKeyCredentials struct {
	Provider string
	APIKey   Secret
	BaseURL  string
	Source   string
}

func (s *Store) ResolveAPIKey(ctx context.Context, provider string) (APIKeyCredentials, error) {
	ctx = s.bind(ctx)
	spec, ok := s.dir.Spec(ctx, provider)
	if !ok || spec.AuthType != AuthTypeAPIKey {
		return APIKeyCredentials{}, providerError(provider, fmt.Sprintf("Provider '%s' is not an API-key provider.", provider), "invalid_provider")
	}
	key, source, err := s.resolveAPIKeySecret(ctx, spec)
	if err != nil {
		return APIKeyCredentials{}, err
	}
	if key == "" && provider == "lmstudio" {
		key = LMStudioNoAuthPlaceholder
		if source == "" {
			source = "default"
		}
	}
	envURL := s.providerEnvBaseURL(ctx, spec)
	baseURL, owned := "", false
	if spec.RuntimeBaseURL != nil {
		baseURL, owned = spec.RuntimeBaseURL(ctx, NewSecret(key), spec.BaseURL, envURL)
	}
	if !owned {
		baseURL = spec.BaseURL
		if envURL != "" {
			baseURL = strings.TrimRight(envURL, "/")
		}
		if provider == "actual" {
			baseURL = NormalizeActualBaseURL(baseURL)
		}
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = spec.BaseURL
	}
	if key == "" && provider == "actual" && IsActualLocalBaseURL(baseURL) {
		key = ActualLocalNoAuthPlaceholder
		if source == "" {
			source = "local-offline"
		}
	}
	if source == "" {
		source = "default"
	}
	return APIKeyCredentials{Provider: provider, APIKey: NewSecret(key), BaseURL: strings.TrimRight(baseURL, "/"), Source: source}, nil
}

type processSpec struct {
	command    string
	args       []string
	baseURL    string
	resolved   string
	commandEnv []string
}

func (s *Store) externalProcessSpec(ctx context.Context, spec SeedSpec) (processSpec, error) {
	out := processSpec{baseURL: s.providerEnvBaseURL(ctx, spec), commandEnv: spec.ProcessCommandEnv}
	if out.baseURL == "" {
		out.baseURL = spec.BaseURL
	}
	for _, name := range spec.ProcessCommandEnv {
		if v := strings.TrimSpace(s.lookupEnv(ctx, name)); v != "" {
			out.command = v
			break
		}
	}
	if out.command == "" {
		out.command = spec.ProcessCommand
	}
	out.args = slices.Clone(spec.ProcessArgs)
	if spec.ProcessArgsEnv != "" {
		if raw := strings.TrimSpace(s.lookupEnv(ctx, spec.ProcessArgsEnv)); raw != "" {
			args, err := shell.Split(raw)
			if err != nil {
				return out, err
			}
			out.args = args
		}
	}
	out.resolved, _ = s.ResolveExternalProcessCommand(ctx, out.command)
	return out, nil
}

func (s *Store) ExternalProcessStatus(ctx context.Context, provider string) (Status, error) {
	ctx = s.bind(ctx)
	spec, ok := s.dir.Spec(ctx, provider)
	if !ok || spec.AuthType != AuthTypeExternalProcess {
		return Status{}, nil
	}
	p, err := s.externalProcessSpec(ctx, spec)
	if err != nil {
		return Status{}, err
	}
	available := p.resolved != "" || strings.HasPrefix(p.baseURL, "acp+tcp://")
	verified, source := p.resolved != "", ""
	if p.resolved != "" {
		source = "command: " + p.resolved
	}
	if spec.ExternalAuthEvidence != nil {
		if v, from, ok := spec.ExternalAuthEvidence(); ok {
			verified, source = v, from
		}
	}
	return Status{
		Configured: available, Provider: provider, Name: spec.Name, Command: p.command, Args: p.args,
		ResolvedCommand: p.resolved, BaseURL: p.baseURL, LoggedIn: available, AuthVerified: verified, AuthSource: source,
	}, nil
}

type ExternalProcessCredentials struct {
	Provider string
	APIKey   Secret
	BaseURL  string
	Command  string
	Args     []string
	Source   string
}

func (s *Store) ResolveExternalProcess(ctx context.Context, provider string) (ExternalProcessCredentials, error) {
	ctx = s.bind(ctx)
	spec, ok := s.dir.Spec(ctx, provider)
	if !ok || spec.AuthType != AuthTypeExternalProcess {
		return ExternalProcessCredentials{}, providerError(provider, fmt.Sprintf("Provider '%s' is not an external-process provider.", provider), "invalid_provider")
	}
	p, err := s.externalProcessSpec(ctx, spec)
	if err != nil {
		return ExternalProcessCredentials{}, err
	}
	if p.resolved == "" && !strings.HasPrefix(p.baseURL, "acp+tcp://") {
		hint := ""
		if len(p.commandEnv) > 0 {
			hint = " or set " + strings.Join(p.commandEnv, "/")
		}
		shown := p.command
		if shown == "" {
			shown = "(none configured)"
		}
		return ExternalProcessCredentials{}, providerError(provider,
			fmt.Sprintf("Could not find the '%s' CLI command '%s'. Install it%s.", provider, shown, hint), "missing_external_process_cli")
	}
	placeholder := spec.Provider
	if placeholder == "" {
		placeholder = provider
	}
	command := p.resolved
	if command == "" {
		command = p.command
	}
	return ExternalProcessCredentials{
		Provider: provider, APIKey: NewSecret(placeholder), BaseURL: strings.TrimRight(p.baseURL, "/"),
		Command: command, Args: p.args, Source: "process",
	}, nil
}

func (s *Store) Status(ctx context.Context, provider string) (Status, error) {
	ctx = s.bind(ctx)
	target := strings.ToLower(strings.TrimSpace(provider))
	if target == "" {
		active, err := s.Active(ctx)
		if err != nil {
			return Status{}, err
		}
		target = strings.ToLower(strings.TrimSpace(active))
	}
	if target == "" {
		return Status{}, nil
	}
	s.mu.Lock()
	registered := s.statuses[target]
	s.mu.Unlock()
	if registered != nil {
		return registered(ctx)
	}
	switch target {
	case ProviderCodex:
		return s.CodexStatus(ctx), nil
	case ProviderXAIOAuth:
		return s.XAIStatus(ctx), nil
	case ProviderQwen:
		return s.QwenStatus(ctx)
	case ProviderMiniMax:
		return s.MiniMaxStatus(ctx)
	}
	spec, ok := s.dir.Spec(ctx, target)
	if !ok {
		return Status{}, nil
	}
	if spec.Status != nil {
		if status, ok := spec.Status(ctx); ok {
			return status, nil
		}
	}
	switch spec.AuthType {
	case AuthTypeExternalProcess:
		return s.ExternalProcessStatus(ctx, target)
	case AuthTypeAPIKey:
		return s.APIKeyStatus(ctx, target)
	}
	return Status{}, nil
}

var oauthGrantDeadCodes = []string{"invalid_grant", "invalid_token", "refresh_token_reused"}

var terminalRefreshCodes = map[string][]string{
	ProviderPortal: {"invalid_grant", "invalid_token", "refresh_token_reused",
		"portal_auth_missing", "portal_auth_missing_access_token", "portal_auth_missing_refresh_token"},
	ProviderCodex:    {"invalid_grant", "invalid_token", "refresh_token_reused", "codex_refresh_failed", "codex_auth_missing_refresh_token"},
	ProviderXAIOAuth: {"xai_refresh_failed", "xai_auth_missing_refresh_token"},
	ProviderQwen:     {},
	ProviderMiniMax:  {},
}

var logoutFromConfig = []string{ProviderPortal, ProviderCodex, ProviderXAIOAuth}

func IsOAuthFlowProvider(provider string) bool {
	_, ok := terminalRefreshCodes[provider]
	return ok
}

func LogoutFallsBackToConfig(provider string) bool {
	return slices.Contains(logoutFromConfig, provider)
}

func IsTerminalRefreshError(err error, provider string) bool {
	var ae *Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Provider == provider && slices.Contains(terminalRefreshCodes[provider], ae.Code) && ae.Relogin
}
