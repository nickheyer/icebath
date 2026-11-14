package provider

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Clears the provider keys the hermes gmi autouse fixture deletes
func clearGmiProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GLM_API_KEY",
		"KIMI_API_KEY", "MINIMAX_API_KEY", "GMI_API_KEY", "GMI_BASE_URL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestGmiAliasesAliasResolves(t *testing.T) {
	for _, alias := range []string{"gmi", "gmi-cloud", "gmicloud"} {
		t.Run(alias, func(t *testing.T) {
			h, ctx, _ := pickerCaseHome(t)
			clearGmiProviderEnv(t)
			t.Setenv("GMI_API_KEY", "gmi-test-key")
			env := testEnv(t, h)
			env.Ctx = ctx
			if got, err := resolveProvider(env, alias); err != nil || got != "gmi" {
				t.Fatalf("resolve_provider(%q) = %q %v", alias, got, err)
			}
		})
	}
}

func TestGmiAliasesProvidersNormalizeProvider(t *testing.T) {
	if NormalizeProvider("gmi-cloud") != "gmi" || NormalizeProvider("gmicloud") != "gmi" {
		t.Fatalf("normalize %q %q", NormalizeProvider("gmi-cloud"), NormalizeProvider("gmicloud"))
	}
}

func TestGmiConfigRegistryOptionalEnvVarsIncludeGmi(t *testing.T) {
	key, ok := config.OptionalEnvVars.Get("GMI_API_KEY")
	if !ok || !key.Password {
		t.Fatalf("GMI_API_KEY %+v %v", key, ok)
	}
	if _, ok := config.OptionalEnvVars.Get("GMI_BASE_URL"); !ok {
		t.Fatal("GMI_BASE_URL missing")
	}
}

func TestGmiModelCatalogProviderModelIdsPrefersLiveApi(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	clearGmiProviderEnv(t)
	swapFlowSeam(t, &apiKeyCredentialsFn, func(_ context.Context, provider string) (string, string) {
		if provider != "gmi" {
			return "", ""
		}
		return "gmi-live-key", "https://api.gmi-serving.com/v1"
	})
	swapFlowSeam(t, &fetchAPIModelsFn, func(_ context.Context, apiKey, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		return []string{"openai/gpt-5.4-mini", "zai-org/GLM-5.1-FP8"}, true
	})
	if got := ProviderModelIDs(ctx, "gmi", false).IDs; !reflect.DeepEqual(got, []string{"openai/gpt-5.4-mini", "zai-org/GLM-5.1-FP8"}) {
		t.Fatalf("ids %v", got)
	}
}

func TestGmiModelMetadataKnownGmiEndpointStillUsesEndpointMetadata(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	swapFlowSeam(t, &fetchEndpointModelMetadataFn, func(context.Context, string, string, bool) map[string]map[string]any {
		return map[string]map[string]any{"anthropic/claude-opus-4.6": {"context_length": 409600}}
	})
	got := GetModelContextLength(ctx, ContextLengthRequest{Model: "anthropic/claude-opus-4.6", BaseURL: "https://api.gmi-serving.com/v1",
		APIKey: "gmi-test-key", Provider: "custom"})
	if got != 409600 {
		t.Fatalf("context length %d", got)
	}
}

func TestGmiAuxiliaryResolveProviderClientUsesGmiAuxDefault(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	clearGmiProviderEnv(t)
	t.Setenv("GMI_API_KEY", "gmi-test-key")
	var kwargsKey any
	var kwargsBase string
	var kwargsHeaders map[string]string
	swapFlowSeam(t, &auxOpenAIConstructorFn, func(apiKey any, baseURL string, opts auxOpenAIOptions) *auxOpenAIClient {
		kwargsKey, kwargsBase, kwargsHeaders = apiKey, baseURL, opts.DefaultHeaders
		return newAuxOpenAIClient(apiKey, baseURL, opts)
	})
	client, model, err := ResolveProviderClient(ctx, "gmi", "", ProviderClientOptions{})
	if err != nil || client == nil || model == "" {
		t.Fatalf("client %v model %q err %v", client, model, err)
	}
	if kwargsKey != "gmi-test-key" || kwargsBase != "https://api.gmi-serving.com/v1" {
		t.Fatalf("key %v base %q", kwargsKey, kwargsBase)
	}
	if ua := kwargsHeaders["User-Agent"]; !strings.HasPrefix(ua, "IcebathAgent/") {
		t.Fatalf("headers %v", kwargsHeaders)
	}
}

func TestGmiMainFlowModelFlowApiKeyProviderPersistsGmiSelection(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	clearGmiProviderEnv(t)
	t.Setenv("GMI_API_KEY", "gmi-test-key")
	swapFlowSeam(t, &fetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"zai-org/GLM-5.1-FP8", "openai/gpt-5.4-mini"}, true
	})
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"zai-org/GLM-5.1-FP8", "openai/gpt-5.4-mini"}, true
	})
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "openai/gpt-5.4-mini", true })
	scriptBuiltinsInput(t, "")
	r := pickerCaseRun(t, ctx, h, out)
	r.CurrentModel = "old-model"
	if err := modelFlowAPIKeyProvider(r, "gmi"); err != nil {
		t.Fatal(err)
	}
	model, ok := readConfig(t, h)["model"].(map[string]any)
	if !ok || model["provider"] != "gmi" || model["default"] != "openai/gpt-5.4-mini" || model["base_url"] != "https://api.gmi-serving.com/v1" {
		t.Fatalf("model %v", readConfig(t, h)["model"])
	}
}

func TestHermesModelDispatchAndEffortPickerConsultTheProfile(t *testing.T) {
	snapshotProviderRegistry(t)
	root := t.TempDir()
	coder := filepath.Join(root, "icebath", "profiles", "coder")
	pluginDir := filepath.Join(coder, "plugins", "model-providers", "acme-device")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(coder, "config.yaml"):     "{}\n",
		filepath.Join(pluginDir, "plugin.yaml"): "name: acme-device\nkind: model-provider\nversion: 0.0.1\ndescription: device login seam fixture\n",
		filepath.Join(pluginDir, "__init__.py"): "# acme-device fixture\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "acme.jsonl")
	logEvent := func(event map[string]any) {
		raw, _ := json.Marshal(event)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_, _ = f.Write(append(raw, '\n'))
	}
	events := func() []map[string]any {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil {
				out = append(out, ev)
			}
		}
		return out
	}
	t.Setenv("ICEBATH_HOME", coder)
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){"acme-device": func(string) ([]Profile, error) {
		return []Profile{{Name: "acme-device", DisplayName: "Acme Device", BaseURL: "https://acme.example/v1", KeyEnv: []string{"ACME_DEVICE_KEY"},
			AccountCard: map[string]string{"name": "Acme Account", "docs_url": "https://acme.example/docs"},
			ModelSetupFlow: func(_ context.Context, _ any, currentModel string, _ any) (any, error) {
				logEvent(map[string]any{"event": "flow", "current_model": currentModel})
				return nil, nil
			},
			SetupReasoningEfforts: func(model string) any {
				if model == "plain" {
					return []any{}
				}
				return []any{"low", "high"}
			}}}, nil
	}})
	ctx := home.WithOverride(context.Background(), coder)
	rediscoverProviders(t, ctx)
	flow := profileModelSetupFlow(ctx, "acme-device")
	if flow == nil {
		t.Fatal("profile flow missing")
	}
	if _, err := flow(ctx, map[string]any{}, "acme-1", nil); err != nil {
		t.Fatal(err)
	}
	var flows []map[string]any
	for _, ev := range events() {
		if ev["event"] == "flow" {
			flows = append(flows, ev)
		}
	}
	if !reflect.DeepEqual(flows, []map[string]any{{"event": "flow", "current_model": "acme-1"}}) {
		t.Fatalf("flow events %v", flows)
	}
	if got, ok, err := mainModelReasoningEfforts(ctx, "acme-1", "acme-device"); err != nil || !ok || !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("efforts %v %v %v", got, ok, err)
	}
	if got, ok, err := mainModelReasoningEfforts(ctx, "plain", "acme-device"); err != nil || ok || got != nil {
		t.Fatalf("plain efforts %v %v %v", got, ok, err)
	}
	if profileModelSetupFlow(ctx, "openrouter") != nil {
		t.Fatal("openrouter has no bespoke step")
	}
}
