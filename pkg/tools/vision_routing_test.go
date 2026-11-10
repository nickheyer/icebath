package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Provider profile flags of the fake capability source
type fakeProfile struct {
	vision, toolMessages bool
}

// Scripted capability source standing in for the pkg/provider wiring
type fakeVisionCaps struct {
	mu          sync.Mutex
	profiles    map[string]fakeProfile
	aggregators map[string]bool
	catalog     map[string]bool
	managed     map[string]bool
	staged      map[string]bool
	local       map[string]bool
	serverType  map[string]string
	ollama      map[string]bool
	catalogAsks []string
	ollamaAsks  []string
}

func (f *fakeVisionCaps) ProviderVisionProfile(provider string) (bool, bool, bool) {
	p, ok := f.profiles[provider]
	return p.vision, p.toolMessages, ok
}
func (f *fakeVisionCaps) IsRoutingAggregator(provider string) bool { return f.aggregators[provider] }
func (f *fakeVisionCaps) CatalogSupportsVision(ctx context.Context, provider, model string) (bool, bool) {
	f.mu.Lock()
	f.catalogAsks = append(f.catalogAsks, provider+"|"+model)
	f.mu.Unlock()
	v, ok := f.catalog[provider+"|"+model]
	return v, ok
}
func (f *fakeVisionCaps) StripCodexContextVariantSuffix(model string) string {
	return strings.TrimSuffix(model, "-900k")
}
func (f *fakeVisionCaps) IsManagedEndpoint(baseURL string) bool {
	return f.managed[strings.TrimRight(baseURL, "/")]
}
func (f *fakeVisionCaps) ManagedModelSupportsVision(ctx context.Context, model string) (bool, bool) {
	v, ok := f.staged[model]
	return v, ok
}
func (f *fakeVisionCaps) IsLocalEndpoint(baseURL string) bool { return f.local[baseURL] }
func (f *fakeVisionCaps) DetectLocalServerType(ctx context.Context, baseURL, apiKey string) string {
	return f.serverType[baseURL]
}
func (f *fakeVisionCaps) QueryOllamaSupportsVision(ctx context.Context, model, baseURL, apiKey string) (bool, bool) {
	f.mu.Lock()
	f.ollamaAsks = append(f.ollamaAsks, model+"|"+baseURL+"|"+apiKey)
	f.mu.Unlock()
	v, ok := f.ollama[model]
	return v, ok
}

// Installs a capability source for one test
func installVisionCaps(t *testing.T, caps *fakeVisionCaps) *fakeVisionCaps {
	t.Helper()
	prev := VisionCapabilities
	VisionCapabilities = caps
	t.Cleanup(func() { VisionCapabilities = prev })
	return caps
}

// Capability source with the xiaomi veto and openrouter aggregator profiles
func standardCaps() *fakeVisionCaps {
	return &fakeVisionCaps{
		profiles: map[string]fakeProfile{
			"xiaomi":     {vision: true, toolMessages: false},
			"openrouter": {vision: true, toolMessages: true},
			"relay":      {vision: true, toolMessages: true},
		},
		aggregators: map[string]bool{"openrouter": true},
		catalog:     map[string]bool{},
	}
}

// Env with a config tree and a bound main runtime
func routingEnv(t *testing.T, cfg config.Tree, runtime map[string]any) *Env {
	t.Helper()
	if cfg == nil {
		cfg = config.Defaults()
	}
	env := &Env{Home: t.TempDir(), Config: cfg, Values: &sync.Map{}}
	if runtime != nil {
		env.Values.Store("runtime_main", runtime)
	}
	return env
}

func TestSupportsMediaInToolResults(t *testing.T) {
	installVisionCaps(t, standardCaps())
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"anthropic", "claude-opus-4-6", true},
		{"openrouter", "anthropic/claude-opus-4.6", true},
		{"", "anything", false},
		{"xiaomi", "mimo-v2.5", false},
		{"gemini", "gemini-2.5-pro", false},
		{"gemini", "gemini-3-pro", true},
		{"relay", "any", true},
		{"brand-new-provider", "x", false},
	}
	for _, c := range cases {
		if got := supportsMediaInToolResults(c.provider, c.model); got != c.want {
			t.Errorf("%s/%s: got %v", c.provider, c.model, got)
		}
	}
}

func TestProfileVetoBeatsVisionCapableLookup(t *testing.T) {
	caps := installVisionCaps(t, standardCaps())
	caps.catalog["xiaomi|mimo-v2.5"] = true
	env := routingEnv(t, nil, map[string]any{"provider": "xiaomi", "model": "mimo-v2.5"})
	if decideImageInputMode(context.Background(), env, "xiaomi", "mimo-v2.5", map[string]any(env.Config), "") != "native" {
		t.Fatal("catalog should route native")
	}
	if ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("tool message veto must win")
	}
	caps.catalog["openrouter|xiaomi/mimo-v2.5"] = true
	env = routingEnv(t, nil, map[string]any{"provider": "openrouter", "model": "xiaomi/mimo-v2.5"})
	if ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("routed xiaomi veto must win through the aggregator")
	}
	caps.catalog["openrouter|anthropic/claude-opus-4.6"] = true
	env = routingEnv(t, nil, map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.6"})
	if !ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("vision capable openrouter route must use the fast path")
	}
}

func TestFastPathGatingByConfig(t *testing.T) {
	installVisionCaps(t, standardCaps())
	env := routingEnv(t, nil, map[string]any{"provider": "brand-new-provider", "model": "anthropic/claude-opus-4.6"})
	if ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("unknown provider must fall through")
	}
	cfg := config.Defaults()
	cfg.Set("model.supports_vision", true)
	env = routingEnv(t, cfg, map[string]any{"provider": "brand-new-provider", "model": "llava-v1.6"})
	if !ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("supports_vision override must enable the fast path")
	}
	cfg.Set("agent.image_input_mode", "text")
	if ShouldUseNativeVisionFastPath(context.Background(), env) {
		t.Fatal("explicit text mode wins")
	}
	cfg = config.Defaults()
	cfg.Set("model.supports_vision", true)
	cfg.Set("auxiliary.vision.model", "google/gemini-flash")
	env = routingEnv(t, cfg, map[string]any{"provider": "anthropic", "model": "claude"})
	if DecideImageInputMode(context.Background(), env, "anthropic", "claude", "") != "text" {
		t.Fatal("explicit auxiliary.vision backend routes text in auto mode")
	}
	cfg.Set("agent.image_input_mode", "native")
	if DecideImageInputMode(context.Background(), env, "anthropic", "claude", "") != "native" {
		t.Fatal("explicit native mode wins")
	}
}

func TestReadMainFieldsFallBackToConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Set("model", map[string]any{"default": " gpt-4o ", "provider": " OpenAI-Codex "})
	env := routingEnv(t, cfg, nil)
	if readMainModel(env) != "gpt-4o" || readMainProvider(env) != "openai-codex" {
		t.Fatalf("config fallback %q %q", readMainModel(env), readMainProvider(env))
	}
	env.Values.Store("runtime_main", map[string]any{"provider": "Anthropic", "model": "claude-opus"})
	if readMainModel(env) != "claude-opus" || readMainProvider(env) != "anthropic" {
		t.Fatal("runtime override must win")
	}
	cfg.Set("model", "plain-model")
	env = routingEnv(t, cfg, nil)
	if readMainModel(env) != "plain-model" || readMainProvider(env) != "" {
		t.Fatal("string model config")
	}
}

func TestCoerceCapabilityBool(t *testing.T) {
	cases := []struct {
		in      any
		v, okay bool
	}{
		{true, true, true}, {false, false, true}, {1, true, true}, {0, false, true}, {2, false, false},
		{"false", false, true}, {" Yes ", true, true}, {"off", false, true}, {"maybe", false, false}, {nil, false, false},
	}
	for _, c := range cases {
		v, ok := coerceCapabilityBool(c.in)
		if v != c.v || ok != c.okay {
			t.Errorf("%v: got %v %v", c.in, v, ok)
		}
	}
}

func TestSupportsVisionOverridePrecedence(t *testing.T) {
	cfg := map[string]any{
		"model":     map[string]any{"provider": "my-local"},
		"providers": map[string]any{"my-local": map[string]any{"models": map[string]any{"llava": map[string]any{"vision": "true"}}}},
		"custom_providers": []any{
			map[string]any{"name": "Other", "models": map[string]any{"qwen-vl": map[string]any{"supports_vision": "false"}}},
		},
	}
	if v, ok := supportsVisionOverride(cfg, "custom", "llava", ""); !ok || !v {
		t.Fatal("providers block through model.provider")
	}
	if v, ok := supportsVisionOverride(cfg, "custom", "qwen-vl", "custom:other"); !ok || v {
		t.Fatal("quoted false must disable through custom_providers")
	}
	if _, ok := supportsVisionOverride(cfg, "custom", "unknown", ""); ok {
		t.Fatal("unset model has no verdict")
	}
	cfg["model"].(map[string]any)["supports_vision"] = "no"
	if v, ok := supportsVisionOverride(cfg, "custom", "llava", ""); !ok || v {
		t.Fatal("top level override wins")
	}
}

func TestLookupSupportsVisionProbes(t *testing.T) {
	caps := installVisionCaps(t, standardCaps())
	caps.managed = map[string]bool{"http://127.0.0.1:8090/v1": true}
	caps.staged = map[string]bool{"gemma-local": true}
	caps.local = map[string]bool{"http://localhost:11434/v1": true}
	caps.serverType = map[string]string{"http://localhost:11434/v1": "ollama"}
	caps.ollama = map[string]bool{"llava:7b": true}
	caps.catalog["openai-codex|gpt-5"] = true
	ctx := context.Background()
	env := routingEnv(t, nil, map[string]any{"provider": "custom", "model": "gemma-local", "base_url": "http://127.0.0.1:8090/v1"})
	if v, ok := lookupSupportsVision(ctx, env, "custom", "gemma-local", map[string]any(env.Config), ""); !ok || !v {
		t.Fatal("managed runtime verdict")
	}
	if v, ok := lookupSupportsVision(ctx, env, "llamacpp", "gemma-local", map[string]any(env.Config), ""); !ok || !v {
		t.Fatal("llamacpp alias is always managed")
	}
	env = routingEnv(t, nil, nil)
	if v, ok := lookupSupportsVision(ctx, env, "openai-codex", "gpt-5-900k", map[string]any(env.Config), ""); !ok || !v {
		t.Fatal("codex variant must map to its base slug")
	}
	if v, ok := lookupSupportsVision(ctx, env, "ollama", "llava:7b", map[string]any(env.Config), ""); !ok || !v {
		t.Fatal("ollama probe with the default local base URL")
	}
	if len(caps.ollamaAsks) != 1 || caps.ollamaAsks[0] != "llava:7b|http://localhost:11434/v1|" {
		t.Fatalf("ollama asks %v", caps.ollamaAsks)
	}
	if _, ok := lookupSupportsVision(ctx, env, "", "model", map[string]any(env.Config), ""); ok {
		t.Fatal("empty provider has no verdict")
	}
	VisionCapabilities = nil
	if _, ok := lookupSupportsVision(ctx, env, "anthropic", "claude", map[string]any(env.Config), ""); ok {
		t.Fatal("no capability source leaves no verdict")
	}
}

func TestResolveInferenceValues(t *testing.T) {
	cfg := map[string]any{
		"model":            map[string]any{"provider": "custom:box"},
		"providers":        map[string]any{"box": map[string]any{"base_url": "http://box:8000/v1", "api_key": "box-key"}},
		"custom_providers": []any{map[string]any{"name": "Box", "base_url": "http://ignored"}},
	}
	env := routingEnv(t, nil, map[string]any{"provider": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key": func() string { return " minted " }})
	if got := resolveInferenceBaseURL(env, cfg, "custom"); got != "http://box:8000/v1" {
		t.Fatalf("runtime base URL of another provider must be ignored, got %q", got)
	}
	if got := resolveInferenceBaseURL(env, cfg, "openrouter"); got != "https://openrouter.ai/api/v1" {
		t.Fatalf("runtime base URL of the same provider, got %q", got)
	}
	if got := resolveInferenceAPIKey(env, cfg, "custom"); got != "minted" {
		t.Fatalf("token source owns authentication, got %q", got)
	}
	env = routingEnv(t, nil, nil)
	if got := resolveInferenceAPIKey(env, cfg, "custom"); got != "box-key" {
		t.Fatalf("providers block key, got %q", got)
	}
}

func TestRequestedProviderBorrowedOnExactRuntimeMatch(t *testing.T) {
	installVisionCaps(t, standardCaps())
	cfg := map[string]any{"providers": map[string]any{"my-box": map[string]any{"models": map[string]any{"vl": map[string]any{"supports_vision": true}}}}}
	env := routingEnv(t, nil, map[string]any{"provider": "custom", "model": "vl", "requested_provider": "my-box"})
	if v, ok := lookupSupportsVision(context.Background(), env, "custom", "vl", cfg, ""); !ok || !v {
		t.Fatal("requested provider identity must be borrowed")
	}
	if _, ok := lookupSupportsVision(context.Background(), env, "custom", "other", cfg, ""); ok {
		t.Fatal("identity borrowed for a different model")
	}
}

func TestWithEnvHomeBindsTheEnvHomeAndKeepsAnUnboundContext(t *testing.T) {
	type key struct{}
	base := home.WithOverride(context.WithValue(context.Background(), key{}, "kept"), "/caller/home")
	bound := WithEnvHome(base, &Env{Home: "/env/home"})
	if got := home.Override(bound); got != "/env/home" {
		t.Fatalf("override %q, want the env home", got)
	}
	if bound.Value(key{}) != "kept" {
		t.Fatal("the caller context values must survive the binding")
	}
	for _, env := range []*Env{nil, {}} {
		if got := WithEnvHome(base, env); got != base {
			t.Fatalf("an env naming no home must hand back the caller context, got %v", got)
		}
	}
	if got := home.Override(WithEnvHome(nil, &Env{Home: "/env/home"})); got != "/env/home" {
		t.Fatalf("a nil context must still carry the env home, got %q", got)
	}
}
