package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

// Temp home for the assignment chain whose seams and config caches are restored when the test ends
func assignHome(t *testing.T) string {
	t.Helper()
	h := lmTestHome(t, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	savedValidate, savedInfo, savedCaps, savedCreds := switchValidateModelFn, switchModelInfoFn, switchCapabilitiesFn, providerHasCredentialsFn
	t.Cleanup(func() {
		switchValidateModelFn, switchModelInfoFn, switchCapabilitiesFn, providerHasCredentialsFn = savedValidate, savedInfo, savedCaps, savedCreds
		config.ResetCache()
	})
	config.ResetCache()
	return h
}

// Writes config text into the home
func assignWrite(t *testing.T, h, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Raw document of the home's config.yaml
func assignRead(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// The model block of the home's config.yaml
func assignModelBlock(t *testing.T, h string) map[string]any {
	t.Helper()
	block, _ := assignRead(t, h)["model"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	return block
}

// Main slot assignment through the model set route body
func assignMain(t *testing.T, provider, model, baseURL, apiKey string) map[string]any {
	t.Helper()
	out, err := ApplyModelAssignmentSync(context.Background(), "main", provider, model, "", baseURL, apiKey, AuxReasoningEffort{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Config load double returning cfg for the rest of the test
func assignLoadReturns(t *testing.T, cfg map[string]any) {
	configLoadFn = func(context.Context) (map[string]any, error) { return cfg, nil }
}

func TestUnresolvedNamedCustomProviderSlugIsPreserved(t *testing.T) {
	assignHome(t)
	assignLoadReturns(t, map[string]any{})
	provider, model := normalizeMainModelAssignment(context.Background(), "custom:litellm", "ollama/glm-5.2")
	if provider != "custom:litellm" || model != "ollama/glm-5.2" {
		t.Fatalf("%q %q", provider, model)
	}
}

func TestKnownNativeProviderStillNormalizesModel(t *testing.T) {
	assignHome(t)
	provider, model := normalizeMainModelAssignment(context.Background(), "anthropic", "anthropic/claude-opus-4.6")
	if provider != "anthropic" || model != "claude-opus-4-6" {
		t.Fatalf("%q %q", provider, model)
	}
}

// Normalization of provider over vendor/model-a with load_config returning cfg
func assignNormalize(t *testing.T, cfg map[string]any, provider string) (string, string) {
	assignLoadReturns(t, cfg)
	return normalizeMainModelAssignment(context.Background(), provider, "vendor/model-a")
}

func TestProvidersBlockKeepsDeclaredBareSlug(t *testing.T) {
	assignHome(t)
	cfg := map[string]any{"providers": map[string]any{"commandcode": map[string]any{"base_url": "http://localhost:55990/v1"}}}
	if p, m := assignNormalize(t, cfg, "commandcode"); p != "commandcode" || m != "vendor/model-a" {
		t.Fatalf("%q %q", p, m)
	}
}

func TestCustomProviderNameCanonicalizesToDurableSlug(t *testing.T) {
	assignHome(t)
	cfg := map[string]any{"custom_providers": []any{map[string]any{"name": "US Azure", "base_url": "http://localhost:18025/v1"}}}
	for _, provider := range []string{"US Azure", "custom:us-azure"} {
		if p, m := assignNormalize(t, cfg, provider); p != "custom:us-azure" || m != "vendor/model-a" {
			t.Fatalf("%s: %q %q", provider, p, m)
		}
	}
}

func TestUnknownVendorStillUsesAggregatorFallback(t *testing.T) {
	assignHome(t)
	providerHasCredentialsFn = func(_ context.Context, p string) bool { return p == "openrouter" }
	if p, m := assignNormalize(t, map[string]any{}, "unconfigured-vendor"); p != "openrouter" || m != "vendor/model-a" {
		t.Fatalf("%q %q", p, m)
	}
}

func TestUnknownVendorWithoutOpenrouterKeyIsNotReassigned(t *testing.T) {
	assignHome(t)
	providerHasCredentialsFn = func(context.Context, string) bool { return false }
	if p, m := assignNormalize(t, map[string]any{}, "unconfigured-vendor"); p != "unconfigured-vendor" || m != "vendor/model-a" {
		t.Fatalf("%q %q", p, m)
	}
}

func TestLocalEndpointPinsAreExcludedFromStaleAuxReport(t *testing.T) {
	assignHome(t)
	cfg := map[string]any{"auxiliary": map[string]any{
		"title_generation": map[string]any{"provider": "openai", "model": "llama3.2:3b", "base_url": "http://byron.local:11434/v1"},
		"vision":           map[string]any{"provider": "openai", "model": "llama3.2-vision:11b", "base_url": "http://192.168.1.10:11434/v1"},
		"compression":      map[string]any{"provider": "openai", "model": "gpt-4o-mini", "base_url": "https://api.example.com/v1"},
		"curator":          map[string]any{"provider": "openai", "model": "gpt-4o-mini"},
		"review":           map[string]any{"provider": "main", "model": "kimi-k3"},
	}}
	var tasks []string
	for _, entry := range staleAuxPins(context.Background(), cfg, "ollama-cloud") {
		tasks = append(tasks, entry["task"].(string))
	}
	sort.Strings(tasks)
	if !reflect.DeepEqual(tasks, []string{"compression", "curator"}) {
		t.Fatalf("stale %v", tasks)
	}
}

// Config text of the env key mirror fixture with the given providers block
func assignMirrorConfig(t *testing.T, h string, providers map[string]any) {
	t.Helper()
	doc := map[string]any{"model": map[string]any{"provider": "openrouter", "default": "some/model"}, "providers": providers}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	assignWrite(t, h, string(raw))
}

func TestEnvRefKeyCarriesTemplateNotPlaintext(t *testing.T) {
	h := assignHome(t)
	t.Setenv("MY_LOCAL_KEY", "sk-super-secret-value")
	assignMirrorConfig(t, h, map[string]any{"mylocal": map[string]any{"base_url": "http://localhost:1234/v1", "api_key": "${MY_LOCAL_KEY}"}})
	assignMain(t, "mylocal", "local/model", "", "")
	block := assignModelBlock(t, h)
	raw, _ := yaml.Marshal(block)
	if strings.Contains(string(raw), "sk-super-secret-value") {
		t.Fatalf("secret leaked %s", raw)
	}
	if block["api_key"] != "${MY_LOCAL_KEY}" {
		t.Fatalf("api_key %v", block["api_key"])
	}
}

func TestKeyEnvEntryCarriesPointer(t *testing.T) {
	h := assignHome(t)
	t.Setenv("MYLOCAL_API_KEY", "sk-keyenv-secret")
	assignMirrorConfig(t, h, map[string]any{"mylocal": map[string]any{"base_url": "http://localhost:1234/v1", "key_env": "MYLOCAL_API_KEY"}})
	assignMain(t, "mylocal", "local/model", "", "")
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if strings.Contains(string(raw), "sk-keyenv-secret") {
		t.Fatalf("secret leaked %s", raw)
	}
	if got := assignModelBlock(t, h)["key_env"]; got != "MYLOCAL_API_KEY" {
		t.Fatalf("key_env %v", got)
	}
}

func TestLiteralKeyStillMirrored(t *testing.T) {
	h := assignHome(t)
	assignMirrorConfig(t, h, map[string]any{"mylocal": map[string]any{"base_url": "http://localhost:1234/v1", "api_key": "literal-key-abc"}})
	assignMain(t, "mylocal", "local/model", "", "")
	if got := assignModelBlock(t, h)["api_key"]; got != "literal-key-abc" {
		t.Fatalf("api_key %v", got)
	}
}

const assignSecret = "sk-SUPERSECRET-e2e-12345"

// Config text before the providers section, the model block of the secret copy fixture
func assignBeforeProviders(t *testing.T, h string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitN(string(raw), "providers:", 2)[0]
}

func TestModelSetSecretHandlingEnvTemplateKeyIsCopiedAsTemplateNotPlaintext(t *testing.T) {
	h := assignHome(t)
	t.Setenv("MY_SECRET_KEY", assignSecret)
	assignWrite(t, h, "model:\n  provider: openrouter\n  default: some/model\n"+
		"providers:\n  myprov:\n    base_url: https://api.example.com/v1\n"+
		"    api_key: ${MY_SECRET_KEY}\n    model: test-model\n")
	assignMain(t, "myprov", "test-model", "", "")
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if strings.Contains(string(raw), assignSecret) {
		t.Fatalf("secret leaked %s", raw)
	}
	if !strings.Contains(assignBeforeProviders(t, h), "api_key: ${MY_SECRET_KEY}") {
		t.Fatalf("model block %s", assignBeforeProviders(t, h))
	}
}

func TestModelSetSecretHandlingKeyEnvPointerIsPreferredOverApiKey(t *testing.T) {
	h := assignHome(t)
	t.Setenv("MY_SECRET_KEY", assignSecret)
	t.Setenv("MYPROV_KEY", assignSecret)
	assignWrite(t, h, "model:\n  provider: openrouter\n  default: some/model\n"+
		"providers:\n  myprov:\n    base_url: https://api.example.com/v1\n"+
		"    key_env: MYPROV_KEY\n    api_key: ${MYPROV_KEY}\n    model: test-model\n")
	assignMain(t, "myprov", "test-model", "", "")
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	block := assignBeforeProviders(t, h)
	if strings.Contains(string(raw), assignSecret) || !strings.Contains(block, "key_env: MYPROV_KEY") || strings.Contains(block, "api_key") {
		t.Fatalf("model block %s", block)
	}
}

func TestModelSetSecretHandlingLiteralOnDiskKeyKeepsLegacyCopyBehavior(t *testing.T) {
	h := assignHome(t)
	t.Setenv("MY_SECRET_KEY", assignSecret)
	assignWrite(t, h, "model:\n  provider: openrouter\n  default: some/model\n"+
		"providers:\n  myprov:\n    base_url: https://api.example.com/v1\n"+
		"    api_key: sk-literal-on-disk\n    model: test-model\n")
	assignMain(t, "myprov", "test-model", "", "")
	if !strings.Contains(assignBeforeProviders(t, h), "api_key: sk-literal-on-disk") {
		t.Fatalf("model block %s", assignBeforeProviders(t, h))
	}
}

func TestDashboardMainSlotWritesTheOneModelBlock(t *testing.T) {
	h := assignHome(t)
	assignWrite(t, h, "model:\n  default: local-model\n  provider: custom\n  base_url: http://localhost:1234/v1\n"+
		"  api_mode: anthropic_messages\n  api_key: sk-stale\n  context_length: 32000\n  model_slots:\n    fast: gpt-5-mini\n"+
		"agent:\n  system_prompt: keepme\n")
	assignmentSwitchModel = func(context.Context, SwitchModelOptions) (ModelSwitchResult, error) {
		return ModelSwitchResult{Success: true, NewModel: "claude-haiku", TargetProvider: "anthropic", ProviderChanged: true,
			APIKey: "sk-new", IsGlobal: true}, nil
	}
	if _, err := ApplyModelAssignmentSync(context.Background(), "main", "anthropic", "claude-haiku", "", "", "", AuxReasoningEffort{}, nil); err != nil {
		t.Fatal(err)
	}
	block := assignModelBlock(t, h)
	if block["default"] != "claude-haiku" || block["provider"] != "anthropic" {
		t.Fatalf("block %v", block)
	}
	for _, key := range []string{"base_url", "api_mode", "context_length", "api_key"} {
		if _, ok := block[key]; ok {
			t.Fatalf("%s lingered %v", key, block)
		}
	}
	if !reflect.DeepEqual(block["model_slots"], map[string]any{"fast": "gpt-5-mini"}) {
		t.Fatalf("slots %v", block["model_slots"])
	}
	if assignRead(t, h)["agent"].(map[string]any)["system_prompt"] != "keepme" {
		t.Fatal("sibling section lost")
	}
}

const (
	assignOldURL       = "http://127.0.0.1:9/old/v1"
	assignNewURL       = "http://127.0.0.1:9/new/v1"
	assignCollidingURL = "http://127.0.0.1:9/v1"
	assignSubmittedURL = "http://127.0.0.2:9/v1"
)

// The two named provider layouts the explicit endpoint cases run over
var assignLayouts = map[string]map[string]any{
	"custom_providers_list": {"custom_providers": []any{map[string]any{"name": "provider-a", "base_url": assignOldURL, "api_key": "key-a", "model": "model-a"}}},
	"providers_dict": {"providers": map[string]any{"custom:provider-a": map[string]any{"name": "provider-a", "base_url": assignOldURL,
		"api_key": "key-a", "default_model": "model-a"}}},
}

// Writes the JSON config of a layout with provider-a active
func assignWriteLayout(t *testing.T, h, layout string, model map[string]any) {
	t.Helper()
	doc := map[string]any{"model": model}
	for k, v := range assignLayouts[layout] {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	assignWrite(t, h, string(raw))
}

// Every endpoint URL the custom_providers rows and providers entries of a config list
func assignEndpointURLs(cfg map[string]any) map[string]bool {
	out := map[string]bool{}
	var rows []any
	if list, ok := cfg["custom_providers"].([]any); ok {
		rows = append(rows, list...)
	}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		for _, v := range providers {
			rows = append(rows, v)
		}
	}
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			out[pyOrStr(m["base_url"])] = true
		}
	}
	return out
}

func TestWizardSaveWithNewUrlRoutesToAndSavesTheNewEndpoint(t *testing.T) {
	for _, layout := range []string{"custom_providers_list", "providers_dict"} {
		for _, apiKey := range []string{"key-b", ""} {
			t.Run(layout+"-"+apiKey, func(t *testing.T) {
				h := assignHome(t)
				assignWriteLayout(t, h, layout, map[string]any{"provider": "custom:provider-a", "default": "model-a"})
				result := assignMain(t, "custom", "model-b", assignNewURL, apiKey)
				if result["provider"] != "custom" || result["base_url"] != assignNewURL {
					t.Fatalf("result %v", result)
				}
				cfg := assignRead(t, h)
				model := cfg["model"].(map[string]any)
				if model["provider"] != "custom" || model["default"] != "model-b" || model["base_url"] != assignNewURL {
					t.Fatalf("model %v", model)
				}
				if pyOrStr(model["api_key"]) != apiKey {
					t.Fatalf("api_key %v", model["api_key"])
				}
				urls := assignEndpointURLs(cfg)
				if !urls[assignOldURL] || !urls[assignNewURL] {
					t.Fatalf("endpoints %v", urls)
				}
			})
		}
	}
}

func TestBareCustomWithoutUrlStillRecoversTheNamedProvider(t *testing.T) {
	h := assignHome(t)
	assignWriteLayout(t, h, "custom_providers_list", map[string]any{"provider": "custom:provider-a", "default": "model-a"})
	ctx := context.Background()
	if provider, _ := normalizeMainModelAssignment(ctx, "custom", "model-a"); provider != "custom:provider-a" {
		t.Fatalf("provider %q", provider)
	}
	cfg, err := configLoadFn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareMainAssignment(ctx, cfg, "custom", "model-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Result.TargetProvider != "custom:provider-a" || prepared.Result.BaseURL != assignOldURL {
		t.Fatalf("result %+v", prepared.Result)
	}
}

// Validation double recording every probe's endpoint and key
type assignProbes struct {
	mu   sync.Mutex
	seen [][2]string
}

func (p *assignProbes) install() {
	switchValidateModelFn = func(ctx context.Context, model, provider string, opts ValidateModelOptions) ModelVerdict {
		p.mu.Lock()
		p.seen = append(p.seen, [2]string{opts.BaseURL, opts.APIKey})
		p.mu.Unlock()
		return ModelVerdict{Accepted: true, Persist: true, Recognized: true}
	}
}

func (p *assignProbes) list() [][2]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]string(nil), p.seen...)
}

func TestWizardValidationProbesTheSubmittedEndpointNotTheSavedOne(t *testing.T) {
	for _, apiKey := range []string{"key-b", ""} {
		t.Run("api_key="+apiKey, func(t *testing.T) {
			h := assignHome(t)
			assignWriteLayout(t, h, "custom_providers_list", map[string]any{"provider": "custom:provider-a", "default": "model-a",
				"base_url": assignOldURL, "api_key": "key-a"})
			probes := &assignProbes{}
			probes.install()
			assignMain(t, "custom", "model-b", assignNewURL, apiKey)
			seen := probes.list()
			if len(seen) == 0 {
				t.Fatal("the submitted endpoint was never validated")
			}
			for _, probe := range seen {
				if probe[0] != assignNewURL || (probe[1] != apiKey && probe[1] != "no-key-required") || probe[1] == "key-a" {
					t.Fatalf("probes %v", seen)
				}
			}
			if got := assignModelBlock(t, h)["base_url"]; got != assignNewURL {
				t.Fatalf("base_url %v", got)
			}
		})
	}
}

func TestProvidersCustomEntryNeverLendsItsKeyToTheSubmittedEndpoint(t *testing.T) {
	for _, apiKey := range []string{"key-b", ""} {
		t.Run("api_key="+apiKey, func(t *testing.T) {
			h := assignHome(t)
			raw, _ := json.Marshal(map[string]any{"model": map[string]any{"provider": "custom", "default": "model-a"},
				"providers": map[string]any{"custom": map[string]any{"base_url": assignCollidingURL, "api_key": "key-a"}}})
			assignWrite(t, h, string(raw))
			probes := &assignProbes{}
			probes.install()
			result := assignMain(t, "custom", "model-b", assignSubmittedURL, apiKey)
			if result["base_url"] != assignSubmittedURL {
				t.Fatalf("result %v", result)
			}
			for _, probe := range probes.list() {
				if probe[0] != assignSubmittedURL || probe[1] == "key-a" {
					t.Fatalf("probes %v", probes.list())
				}
			}
			model := assignModelBlock(t, h)
			if model["base_url"] != assignSubmittedURL || pyOrStr(model["api_key"]) != apiKey {
				t.Fatalf("model %v", model)
			}
		})
	}
}

func TestBareCustomWithoutUrlStillSelectsTheProvidersCustomEntry(t *testing.T) {
	h := assignHome(t)
	raw, _ := json.Marshal(map[string]any{"model": map[string]any{"provider": "openrouter", "default": "model-a"},
		"providers": map[string]any{"custom": map[string]any{"base_url": assignCollidingURL, "api_key": "key-a"}}})
	assignWrite(t, h, string(raw))
	assignMain(t, "custom", "model-b", "", "")
	model := assignModelBlock(t, h)
	if model["base_url"] != assignCollidingURL || pyOrStr(model["api_key"]) != "key-a" {
		t.Fatalf("model %v", model)
	}
}

const assignCLIConfig = "model:\n  default: gemma4:latest\n  provider: custom\n  base_url: http://10.0.0.155:11434/v1\n  api_mode: chat_completions\n"

func TestSameEndpointModelSetPreservesBareCustomBlock(t *testing.T) {
	h := assignHome(t)
	assignWrite(t, h, assignCLIConfig)
	assignMain(t, "custom", "gemma4:latest", "http://10.0.0.155:11434/v1", "")
	model := assignModelBlock(t, h)
	if model["default"] != "gemma4:latest" || model["provider"] != "custom" || model["base_url"] != "http://10.0.0.155:11434/v1" ||
		model["api_mode"] != "chat_completions" {
		t.Fatalf("model %v", model)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if strings.Contains(string(raw), "custom_providers") {
		t.Fatalf("registered a known endpoint %s", raw)
	}
}

func TestNewEndpointModelSetStillRegistersNamedProvider(t *testing.T) {
	h := assignHome(t)
	assignWrite(t, h, assignCLIConfig)
	assignMain(t, "custom", "other-model", "http://192.168.1.50:11434/v1", "")
	cfg := assignRead(t, h)
	registered := false
	rows, _ := cfg["custom_providers"].([]any)
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m["base_url"] == "http://192.168.1.50:11434/v1" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("custom_providers %v", cfg["custom_providers"])
	}
	if got := assignModelBlock(t, h)["base_url"]; got != "http://192.168.1.50:11434/v1" {
		t.Fatalf("base_url %v", got)
	}
}

// Save double recording every saved section into store
func assignSaved(t *testing.T) map[string]any {
	store := map[string]any{}
	configSaveFn = func(ctx context.Context, cfg map[string]any) error {
		for k, v := range cfg {
			store[k] = v
		}
		return nil
	}
	return store
}

// Reasoning effort field set to a level
func assignEffort(level string) AuxReasoningEffort {
	return AuxReasoningEffort{Present: true, Value: &level}
}

func TestReasoningEffortFieldSemanticsOmittedNullAndLevel(t *testing.T) {
	assignHome(t)
	saved := assignSaved(t)
	ctx := context.Background()
	cfg := map[string]any{"auxiliary": map[string]any{"vision": map[string]any{"provider": "openrouter", "model": "m1", "reasoning_effort": "low"}}}
	aux := func() map[string]any { return cfg["auxiliary"].(map[string]any) }
	if _, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "m2", "vision", "", "", AuxReasoningEffort{}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"provider": "openrouter", "model": "m2", "reasoning_effort": "low"}; !reflect.DeepEqual(aux()["vision"], want) {
		t.Fatalf("vision %v", aux()["vision"])
	}
	out, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "m2", "vision", "", "", assignEffort("HIGH"))
	if err != nil {
		t.Fatal(err)
	}
	if aux()["vision"].(map[string]any)["reasoning_effort"] != "high" || out["reasoning_effort"] != "high" {
		t.Fatalf("vision %v out %v", aux()["vision"], out)
	}
	if _, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "m2", "vision", "", "", assignEffort("disabled")); err != nil {
		t.Fatal(err)
	}
	if aux()["vision"].(map[string]any)["reasoning_effort"] != "none" {
		t.Fatalf("vision %v", aux()["vision"])
	}
	aux()["compression"] = map[string]any{"provider": "openrouter", "model": "m3", "reasoning_effort": "max"}
	if _, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "m2", "vision", "", "", AuxReasoningEffort{Present: true}); err != nil {
		t.Fatal(err)
	}
	vision := aux()["vision"].(map[string]any)
	if _, ok := vision["reasoning_effort"]; ok || vision["model"] != "m2" {
		t.Fatalf("vision %v", vision)
	}
	if aux()["compression"].(map[string]any)["reasoning_effort"] != "max" {
		t.Fatalf("compression %v", aux()["compression"])
	}
	if !reflect.DeepEqual(saved["auxiliary"], cfg["auxiliary"]) {
		t.Fatalf("saved %v", saved["auxiliary"])
	}
}

func TestUnknownLevelIsRejectedAndResetClearsOverrides(t *testing.T) {
	assignHome(t)
	assignSaved(t)
	ctx := context.Background()
	cfg := map[string]any{"auxiliary": map[string]any{"vision": map[string]any{"provider": "openrouter", "model": "m1"}}}
	_, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "m1", "vision", "", "", assignEffort("turbo"))
	if lmStatusOf(err) != 400 || !strings.Contains(err.(*HTTPException).Detail, "reasoning_effort") {
		t.Fatalf("turbo %v", err)
	}
	vision := cfg["auxiliary"].(map[string]any)["vision"].(map[string]any)
	if _, ok := vision["reasoning_effort"]; ok {
		t.Fatalf("vision %v", vision)
	}
	vision["reasoning_effort"] = "high"
	if _, err := applyAuxAssignmentSync(ctx, cfg, "", "", "__reset__", "", "", AuxReasoningEffort{}); err != nil {
		t.Fatal(err)
	}
	for slot, raw := range cfg["auxiliary"].(map[string]any) {
		if _, ok := raw.(map[string]any)["reasoning_effort"]; ok {
			t.Fatalf("%s kept %v", slot, raw)
		}
	}
}

func TestSubmittedCustomEndpointSurvivesAssignmentSubmittedCustomEndpointWinsOverAnEnvEndpoint(t *testing.T) {
	assignHome(t)
	t.Setenv("CUSTOM_BASE_URL", "http://127.0.0.1:9999/v1")
	switchValidateModelFn = func(context.Context, string, string, ValidateModelOptions) ModelVerdict {
		return ModelVerdict{Accepted: true, Persist: true, Recognized: true}
	}
	switchModelInfoFn = func(context.Context, string, string, bool) (ModelsDevEntry, bool) { return ModelsDevEntry{}, false }
	switchCapabilitiesFn = func(context.Context, string, string, bool) (ModelCapabilities, bool) {
		return ModelCapabilities{}, false
	}
	ctx := context.Background()
	cfg := map[string]any{"model": map[string]any{"provider": "openrouter", "default": "m"}}
	result, err := validatedMainModelSelection(ctx, cfg, "custom", "qwen3:8b", "https://api.anthropic.com", "submitted-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseURL != "https://api.anthropic.com" || result.APIMode != "anthropic_messages" {
		t.Fatalf("result %+v", result)
	}
	applied, err := applyMainModelAssignment(ctx, cfg["model"], result, "submitted-key")
	if err != nil {
		t.Fatal(err)
	}
	if applied["base_url"] != "https://api.anthropic.com" || applied["api_mode"] != "anthropic_messages" || applied["api_key"] != "submitted-key" {
		t.Fatalf("applied %v", applied)
	}
}

func TestDashboardMainAssignmentRejectsWhatSwitchModelRejects(t *testing.T) {
	assignHome(t)
	assignmentSwitchModel = func(context.Context, SwitchModelOptions) (ModelSwitchResult, error) {
		return ModelSwitchResult{Success: false, ErrorMessage: "Unknown provider 'notaprovider'."}, nil
	}
	_, err := ApplyModelAssignmentSync(context.Background(), "main", "notaprovider", "whatever", "", "", "", AuxReasoningEffort{}, nil)
	if lmStatusOf(err) != 400 || !strings.Contains(err.(*HTTPException).Detail, "Unknown provider") {
		t.Fatalf("assignment %v", err)
	}
}

func TestInferProviderOnModelChangeSignalsInOrder(t *testing.T) {
	assignHome(t)
	ctx := context.Background()
	if p, m := inferProviderOnModelChange(ctx, "  ", "openrouter"); p != "" || m != "" {
		t.Fatalf("blank %q %q", p, m)
	}
	providerHasCredentialsFn = func(_ context.Context, p string) bool { return p == "openrouter" }
	if p, m := inferProviderOnModelChange(ctx, "zz-vendor/zz-model", "ollama-local"); p != "openrouter" || m != "zz-vendor/zz-model" {
		t.Fatalf("vendor slug %q %q", p, m)
	}
	if p, m := inferProviderOnModelChange(ctx, "zz-vendor/zz-model", "openrouter"); p != "" || m != "zz-vendor/zz-model" {
		t.Fatalf("aggregator %q %q", p, m)
	}
	providerHasCredentialsFn = func(context.Context, string) bool { return false }
	if p, m := inferProviderOnModelChange(ctx, "zz-vendor/zz-model", "ollama-local"); p != "" || m != "zz-vendor/zz-model" {
		t.Fatalf("no key %q %q", p, m)
	}
}
