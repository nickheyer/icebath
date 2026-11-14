package providers

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/auth/aws"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var bedrockOpenAIProfilePrefixes = []string{"global", "us", "eu", "apac", "ap", "au", "jp", "ca", "sa", "me", "af", "in"}

var BedrockOpenAIResponsesModelIDs = []string{
	"openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna",
	"openai.gpt-5.5", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.6-luna",
}

var bedrockMantleHost = regexp.MustCompile(`(?i)^bedrock-mantle\.([a-z0-9-]+)\.api\.aws$`)

type bedrockChainKey struct {
	home       string
	noMetadata bool
}

type bedrockChain struct {
	scope aws.Scope
	chain *aws.Chain
}

type bedrockProfile struct {
	base
	d      Deps
	mu     sync.Mutex
	chains map[bedrockChainKey]bedrockChain
}

type BedrockOwner interface {
	Bedrock(ctx context.Context, instanceMetadata bool) (*BedrockAccess, error)
}

type BedrockAccess struct {
	chain *aws.Chain
	env   aws.Env
	cfg   config.BedrockConfig
}

func newBedrock(d Deps) Profile {
	return &bedrockProfile{d: d, chains: map[bedrockChainKey]bedrockChain{}, base: base{spec: Spec{
		Name: "bedrock", Aliases: []string{"aws", "aws-bedrock", "amazon-bedrock", "amazon"}, Mode: wire.ModeBedrock,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", AuthType: authTypeAWSSDK, SkipModelListing: true,
	}}}
}

func (p *bedrockProfile) Bedrock(ctx context.Context, instanceMetadata bool) (*BedrockAccess, error) {
	detached := context.WithoutCancel(ctx)
	env := func(name string) string { return p.d.secret(detached, name) }
	scope, err := aws.Scoped(env, p.d.multiplexed(ctx))
	if err != nil {
		return nil, err
	}
	dir, err := p.d.homeDir(ctx)
	if err != nil {
		return nil, err
	}
	var cfg config.BedrockConfig
	if p.d.Config != nil {
		if loaded, err := p.d.Config.LoadReadonly(ctx); err == nil {
			cfg = loaded.Bedrock
		}
	}
	key := bedrockChainKey{home: dir, noMetadata: !instanceMetadata}
	p.mu.Lock()
	defer p.mu.Unlock()
	if held, ok := p.chains[key]; ok && held.scope == scope {
		return &BedrockAccess{chain: held.chain, env: env, cfg: cfg}, nil
	}
	chain, err := aws.NewChain(aws.Options{Scope: scope, NoInstanceMetadata: !instanceMetadata, HTTP: p.d.HTTP})
	if err != nil {
		return nil, err
	}
	p.chains[key] = bedrockChain{scope: scope, chain: chain}
	return &BedrockAccess{chain: chain, env: env, cfg: cfg}, nil
}

func (p *bedrockProfile) AuthStatus(ctx context.Context) (auth.Status, bool) {
	return awsSDKStatus(ctx, p, p.spec.Name), true
}

func awsSDKStatus(ctx context.Context, owner BedrockOwner, provider string) auth.Status {
	access, err := owner.Bedrock(ctx, true)
	if err != nil {
		return auth.Status{Provider: provider, Error: err.Error()}
	}
	return auth.Status{Provider: provider, LoggedIn: access.HasCredentials(ctx)}
}

func (a *BedrockAccess) WithConfig(cfg config.BedrockConfig) *BedrockAccess {
	scoped := *a
	scoped.cfg = cfg
	return &scoped
}

func (a *BedrockAccess) Source(ctx context.Context) string { return a.chain.Source(ctx, a.env) }

func (a *BedrockAccess) HasCredentials(ctx context.Context) bool {
	return a.chain.HasCredentials(ctx, a.env)
}

func (a *BedrockAccess) Region() string { return aws.Region(a.env) }

func (a *BedrockAccess) RuntimeRegion() string { return aws.RuntimeRegion(a.cfg, a.env) }

func (a *BedrockAccess) BearerToken() auth.Secret { return auth.NewSecret(aws.BearerToken(a.env)) }

func (a *BedrockAccess) signerRegion(region string) string {
	if resolved := strings.TrimSpace(region); resolved != "" {
		return resolved
	}
	return a.RuntimeRegion()
}

func (a *BedrockAccess) Signer(region string) wire.Authorizer {
	return a.chain.Signer(a.signerRegion(region), a.env)
}

func (a *BedrockAccess) SigV4(region string) wire.Authorizer {
	return a.chain.Signer(a.signerRegion(region), nil)
}

func (a *BedrockAccess) Endpoint(service, region string) (string, error) {
	return a.chain.Endpoint(service, a.signerRegion(region))
}

func (a *BedrockAccess) OpenAIBaseURL(region string) string {
	return BedrockOpenAIBaseURL(region, a.RuntimeRegion())
}

func BedrockGuardrail(cfg config.BedrockConfig) *wire.Guardrail {
	g := cfg.Guardrail
	if g.GuardrailIdentifier == "" || g.GuardrailVersion == "" {
		return nil
	}
	return &wire.Guardrail{ID: g.GuardrailIdentifier, Version: g.GuardrailVersion,
		Trace: string(g.Trace), StreamProcessing: string(g.StreamProcessingMode)}
}

func BedrockGuardrailHeaders(g *wire.Guardrail) map[string]string {
	if g == nil || g.ID == "" || g.Version == "" {
		return map[string]string{}
	}
	headers := map[string]string{
		"X-Amzn-Bedrock-GuardrailIdentifier": g.ID,
		"X-Amzn-Bedrock-GuardrailVersion":    g.Version,
	}
	if slices.Contains([]string{"enabled", "enabled_full", "true"}, strings.ToLower(g.Trace)) {
		headers["X-Amzn-Bedrock-Trace"] = "ENABLED"
	}
	return headers
}

func ParseBedrockOpenAIModelID(modelID string) (bare, profilePrefix string, ok bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	prefix, rest, found := strings.Cut(normalized, ".")
	if found && slices.Contains(bedrockOpenAIProfilePrefixes, prefix) && strings.HasPrefix(rest, "openai.") {
		normalized = rest
	} else {
		prefix = ""
	}
	if slices.ContainsFunc(BedrockOpenAIResponsesModelIDs, func(m string) bool { return strings.ToLower(m) == normalized }) {
		return normalized, prefix, true
	}
	return "", "", false
}

func IsOpenAIBedrockModel(modelID string) bool {
	_, _, ok := ParseBedrockOpenAIModelID(modelID)
	return ok
}

func BedrockOpenAIUsesMantle(modelID string) bool {
	_, prefix, ok := ParseBedrockOpenAIModelID(modelID)
	return ok && prefix == ""
}

func MergeBedrockOpenAIModelIDs(ids []string) []string {
	merged := slices.Clone(ids)
	seen := map[string]bool{}
	for _, m := range merged {
		seen[strings.ToLower(m)] = true
	}
	for _, m := range BedrockOpenAIResponsesModelIDs {
		if !seen[strings.ToLower(m)] {
			merged = append(merged, m)
		}
	}
	return merged
}

func BedrockOpenAIBaseURL(region, runtimeRegion string) string {
	resolved := strings.TrimSpace(region)
	if resolved == "" {
		resolved = runtimeRegion
	}
	return "https://bedrock-mantle." + resolved + ".api.aws/openai/v1"
}

func mantleURLParts(baseURL string) (string, string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", "", false
	}
	m := bedrockMantleHost.FindStringSubmatch(u.Hostname())
	path := strings.ToLower(strings.TrimRight(u.Path, "/"))
	if m == nil {
		return "", path, false
	}
	return m[1], path, true
}

func BedrockOpenAIRegionFromBaseURL(baseURL string) (string, bool) {
	region, _, ok := mantleURLParts(baseURL)
	return region, ok
}

func IsBedrockOpenAIBaseURL(baseURL string) bool {
	_, path, ok := mantleURLParts(baseURL)
	return ok && (path == "" || path == "/openai" || path == "/openai/v1")
}

const BedrockSDKKey = "aws-sdk"

type BedrockBinding struct {
	Region    string
	BaseURL   string
	Mode      wire.Mode
	APIKey    string
	Guardrail *wire.Guardrail
	Headers   map[string]string
}

func BindBedrockRuntime(baseURL string, mode wire.Mode, cfg config.BedrockConfig) BedrockBinding {
	b := BedrockBinding{Region: aws.RegionFromRuntimeURL(baseURL), BaseURL: baseURL, Mode: mode, APIKey: BedrockSDKKey,
		Guardrail: BedrockGuardrail(cfg)}
	if mode == wire.ModeAnthropic {
		b.Headers = BedrockGuardrailHeaders(b.Guardrail)
	}
	return b
}

func BedrockModelIDs(ctx context.Context, region string, discover func(ctx context.Context, region string) ([]string, error)) ([]string, bool) {
	ids, err := discover(ctx, region)
	if err != nil || len(ids) == 0 {
		return nil, false
	}
	return MergeBedrockOpenAIModelIDs(ids), true
}
