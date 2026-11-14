package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
	"github.com/nickheyer/icebath/internal/model/wire/bedrock"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

type Options struct {
	Home             string
	Config           *config.Store
	HTTP             *httpclient.Transport
	Auth             *auth.Store
	Secrets          *secret.Scope
	Providers        *providers.Registry
	Catalog          *catalog.Catalog
	Portal           *portal.Account
	Local            *local.Runtime
	Hooks            Hooks
	Dialects         func(ctx context.Context) []wire.Mode
	PrefixedModel    func(ctx context.Context, provider, model string) string
	Canonical        func(ctx context.Context) []providers.CanonicalEntry
	InferenceProfile func(arn string) string
	DiskFull         func(err error) bool
	RecoverProvider  func(ctx context.Context, provider string)
	ClientEnv        func(ctx context.Context) (map[string]string, bool)
	Version          string
	HTTPOptions      httpclient.Options
	MoAAggregator    func(ctx context.Context, model string) (provider, aggModel string, ok bool)
	SearchProvider   func(ctx context.Context) (string, error)
	Callers          Callers
	SpillToolOutput  func(ctx context.Context, content, toolName, callID string, threshold int) string
	DumpOutput       io.Writer
	LargeModelNotice func(ctx context.Context, model, provider string) string
}

type Callers struct {
	MoA      wire.Caller
	Process  func(ctx context.Context, rt *Route) (wire.Caller, error)
	Dialects map[wire.Mode]wire.Caller
}

type Router struct {
	c      *core
	frozen *frozenConfig
}

type core struct {
	o          Options
	classifier *Classifier
	estimator  *Estimator
	images     *ImageCosts
	prefixes   *stablePrefixes
	responses  *openai.ResponsesProtocol
	anthropic  *anthropic.Protocol
	bedrock    *bedrock.Protocol
	mu         sync.Mutex
	transports map[string]*httpclient.Transport
	warned     map[string]bool
}

func (c *core) warnOnce(key, message string) {
	c.mu.Lock()
	first := !c.warned[key]
	c.warned[key] = true
	c.mu.Unlock()
	if first {
		slog.Warn(message)
	}
}

type frozenConfig struct {
	cfg     *config.Config
	custom  []config.CustomEndpoint
	entries []auth.CustomEntry
}

func New(o Options) (*Router, error) {
	switch {
	case strings.TrimSpace(o.Home) == "":
		return nil, errors.New("model: router needs the profile home")
	case o.Config == nil:
		return nil, errors.New("model: router needs the config store")
	case o.HTTP == nil:
		return nil, errors.New("model: router needs the HTTP transport")
	case o.Auth == nil:
		return nil, errors.New("model: router needs the credential store")
	case o.Secrets == nil:
		return nil, errors.New("model: router needs the secret scope")
	case o.Providers == nil:
		return nil, errors.New("model: router needs the provider registry")
	case o.Catalog == nil:
		return nil, errors.New("model: router needs the model catalog")
	case o.Portal == nil:
		return nil, errors.New("model: router needs the portal account")
	case o.Local == nil:
		return nil, errors.New("model: router needs the local runtime")
	case o.Dialects == nil:
		return nil, errors.New("model: router needs the registered dialect list")
	case o.PrefixedModel == nil:
		return nil, errors.New("model: router needs the prefixed model id lookup")
	case o.Canonical == nil:
		return nil, errors.New("model: router needs the canonical provider list")
	case o.InferenceProfile == nil:
		return nil, errors.New("model: router needs the inference profile resolver")
	case o.DiskFull == nil:
		return nil, errors.New("model: router needs the disk full predicate")
	case o.RecoverProvider == nil:
		return nil, errors.New("model: router needs the left-core provider recovery")
	case o.ClientEnv == nil:
		return nil, errors.New("model: router needs the session client environment")
	case strings.TrimSpace(o.Version) == "":
		return nil, errors.New("model: router needs the product base version")
	case o.MoAAggregator == nil:
		return nil, errors.New("model: router needs the mixture-of-agents aggregator lookup")
	case o.SearchProvider == nil:
		return nil, errors.New("model: router needs the active web search provider lookup")
	case o.SpillToolOutput == nil:
		return nil, errors.New("model: router needs the tool output spill for oversized retry payloads")
	case o.DumpOutput == nil:
		return nil, errors.New("model: router needs the standard output writer for request dumps")
	case o.LargeModelNotice == nil:
		return nil, errors.New("model: router needs the plan's large model notice for model switches")
	}
	r := &Router{c: &core{o: o, prefixes: &stablePrefixes{}, responses: &openai.ResponsesProtocol{}, anthropic: &anthropic.Protocol{},
		bedrock: &bedrock.Protocol{}, transports: map[string]*httpclient.Transport{}, warned: map[string]bool{}}}
	classifier, err := NewClassifier(ClassifierOptions{
		Providers: o.Providers,
		Hooks:     o.Hooks,
		OwnsRoute: func(ctx context.Context, provider, baseURL string) (bool, bool) {
			return r.OwnsRoute(ctx, provider, baseURL, nil)
		},
		PrefixedModel: o.PrefixedModel,
		Label:         o.Catalog.Label,
		Descriptor:    r.descriptor,
		DiskFull:      o.DiskFull,
	})
	if err != nil {
		return nil, err
	}
	estimator, err := NewEstimator(EstimatorOptions{Providers: o.Providers, ResolveProfile: o.InferenceProfile})
	if err != nil {
		return nil, err
	}
	r.c.classifier, r.c.estimator, r.c.images = classifier, estimator, NewImageCosts(o.Home)
	return r, nil
}

func (r *Router) Classifier() *Classifier { return r.c.classifier }

func (r *Router) Estimator() *Estimator { return r.c.estimator }

func (r *Router) ImageCosts() *ImageCosts { return r.c.images }

func (r *Router) descriptor(ctx context.Context, provider string) (providers.Descriptor, bool) {
	ctx = r.bind(ctx)
	for _, d := range r.c.o.Providers.Descriptors(ctx, r.c.o.Canonical(ctx), r.c.o.Config.EnvRegistry()) {
		if d.Slug == provider {
			return d, true
		}
	}
	return providers.Descriptor{}, false
}

func (r *Router) bind(ctx context.Context) context.Context {
	return home.WithOverride(ctx, r.c.o.Home)
}

func (r *Router) Frozen(cfg *config.Config) (*Router, error) {
	if cfg == nil {
		return nil, errors.New("model: a frozen route needs its config")
	}
	tree, err := config.ValueNode(cfg)
	if err != nil {
		return nil, fmt.Errorf("model: freezing the route config: %w", err)
	}
	custom := r.c.o.Config.CustomEndpointsOf(tree)
	if custom == nil {
		custom = []config.CustomEndpoint{}
	}
	entries := []auth.CustomEntry{}
	for _, c := range custom {
		if c.Text("name") != "" {
			entries = append(entries, customEntryOf(c))
		}
	}
	return &Router{c: r.c, frozen: &frozenConfig{cfg: cfg, custom: custom, entries: entries}}, nil
}

func customEntryOf(c config.CustomEndpoint) auth.CustomEntry {
	e := auth.CustomEntry{
		Name: normalizeCustomName(c.Text("name")), DisplayName: c.Text("name"), ProviderKey: c.Text("provider_key"),
		BaseURL: c.Text("base_url"), APIKey: auth.NewSecret(c.Text("api_key")), KeyEnv: c.Text("key_env"),
		APIKeyEnv: c.Text("api_key_env"), KeyCmd: c.Text("key_cmd"),
	}
	e.DeclaresCredential = strings.TrimSpace(e.APIKey.Reveal()) != "" || strings.TrimSpace(e.KeyEnv) != "" ||
		strings.TrimSpace(e.APIKeyEnv) != "" || strings.TrimSpace(e.KeyCmd) != ""
	return e
}

type deferMintKey struct{}

func WithDeferredMint(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferMintKey{}, true)
}

func deferredMint(ctx context.Context) bool {
	v, _ := ctx.Value(deferMintKey{}).(bool)
	return v
}

type Homes struct {
	homes   *home.Resolver
	build   func(ctx context.Context, homeDir string) (*Router, error)
	mu      sync.Mutex
	routers map[string]*Router
}

func NewHomes(homes *home.Resolver, build func(ctx context.Context, homeDir string) (*Router, error)) (*Homes, error) {
	if homes == nil || build == nil {
		return nil, errors.New("model: the router set needs a home resolver and a router constructor")
	}
	return &Homes{homes: homes, build: build, routers: map[string]*Router{}}, nil
}

func (h *Homes) For(ctx context.Context) (*Router, error) {
	dir, err := h.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	key, err := h.homes.Key(dir)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.routers[key]; ok {
		return r, nil
	}
	r, err := h.build(home.WithOverride(ctx, dir), dir)
	if err != nil {
		return nil, err
	}
	h.routers[key] = r
	return r, nil
}

type Target struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   auth.Secret
	Tokens   auth.TokenSource
}

func (t Target) String() string {
	return fmt.Sprintf("Target{Provider:%s Model:%s BaseURL:%s}", t.Provider, t.Model, t.BaseURL)
}

func (t Target) GoString() string { return t.String() }

func (t Target) LogValue() slog.Value {
	return slog.GroupValue(slog.String("provider", t.Provider), slog.String("model", t.Model), slog.String("base_url", t.BaseURL))
}

type Route struct {
	Provider         string
	Requested        string
	Mode             wire.Mode
	BaseURL          string
	Model            string
	Credential       auth.Credential
	Pool             *auth.Pool
	Source           string
	ExpiresAt        time.Time
	LastRefresh      time.Time
	AuthErr          error
	Command          string
	Args             []string
	Region           string
	Guardrail        *wire.Guardrail
	BedrockOpenAI    bool
	BedrockAnthropic bool
	AuthMode         string
	EntraScope       string
	Capabilities     map[string]bool
	Headers          map[string]string
	ExtraBody        map[string]json.RawMessage
}

func (rt *Route) String() string {
	if rt == nil {
		return "Route<nil>"
	}
	return fmt.Sprintf("Route{Provider:%s Mode:%s BaseURL:%s Source:%s}", rt.Provider, rt.Mode, rt.BaseURL, rt.Source)
}

func (rt *Route) GoString() string { return rt.String() }

func (rt *Route) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(rt.String())) }

func (rt *Route) LogValue() slog.Value {
	if rt == nil {
		return slog.StringValue("Route<nil>")
	}
	return slog.GroupValue(slog.String("provider", rt.Provider), slog.String("mode", string(rt.Mode)),
		slog.String("base_url", rt.BaseURL), slog.String("source", rt.Source))
}

type ProviderDisabledError struct {
	Provider string
}

func (e *ProviderDisabledError) Error() string {
	return fmt.Sprintf("provider '%s' is disabled in config (providers.%s.enabled: false)", e.Provider, e.Provider)
}

type LocalRuntimeUnavailableError struct {
	Message   string
	Retryable bool
}

func (e *LocalRuntimeUnavailableError) Error() string { return e.Message }

func (e *LocalRuntimeUnavailableError) Code() string { return "local_runtime_unavailable" }

func authError(message, provider, code string) *auth.Error {
	return &auth.Error{Message: message, Provider: provider, Code: code}
}

func IsAuthError(err error) bool {
	var ae *auth.Error
	return errors.As(err, &ae)
}

func noKey(source string) auth.Credential {
	return auth.Credential{Kind: auth.CredNone, Source: source}
}

func staticKey(key, source string) auth.Credential {
	if key == "" {
		return noKey(source)
	}
	return auth.Credential{Kind: auth.CredAPIKey, Key: auth.NewSecret(key), Source: source}
}

func tokenKey(tokens auth.TokenSource, source string) auth.Credential {
	return auth.Credential{Kind: auth.CredToken, Tokens: tokens, Source: source}
}

func keyText(c auth.Credential) string {
	if c.Kind != auth.CredAPIKey && c.Kind != auth.CredXAPIKey {
		return ""
	}
	return c.Key.Reveal()
}

func keyPresent(c auth.Credential) bool {
	switch c.Kind {
	case auth.CredToken:
		return c.Tokens != nil
	case auth.CredSigned:
		return c.Sign != nil
	case auth.CredNone:
		return false
	}
	return c.Key.Reveal() != ""
}

func usableKey(c auth.Credential) bool {
	if c.Kind != auth.CredAPIKey && c.Kind != auth.CredXAPIKey {
		return false
	}
	return auth.HasUsableSecret(c.Key.Reveal(), 4)
}

func normalizeCustomName(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), " ", "-")
}

func yamlChild(n *yaml.Node, key string) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			v := n.Content[i+1]
			for v != nil && v.Kind == yaml.AliasNode {
				v = v.Alias
			}
			return v
		}
	}
	return nil
}

func jsonOfNode(n *yaml.Node) json.RawMessage {
	return json.RawMessage(config.FormatValue(n, true))
}

func boolMapOfNode(n *yaml.Node) map[string]bool {
	out := map[string]bool{}
	if n == nil || n.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		for v != nil && v.Kind == yaml.AliasNode {
			v = v.Alias
		}
		if k.Kind != yaml.ScalarNode || v == nil || v.Kind != yaml.ScalarNode {
			continue
		}
		var ks, vs config.Scalar
		if k.Decode(&ks) != nil || ks.Kind() != config.KindString || v.Decode(&vs) != nil {
			continue
		}
		if b, ok := vs.AsBool(); ok {
			out[k.Value] = b
		}
	}
	return out
}

func objectMapOfNode(n *yaml.Node) map[string]json.RawMessage {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = jsonOfNode(n.Content[i+1])
	}
	return out
}
