package provider

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Answers line_input and builtin input from one queue, the last answer repeating like a patched return value
func scriptBuiltinsInput(t *testing.T, answers ...string) {
	t.Helper()
	next := func() (string, error) {
		if len(answers) == 0 {
			return "", io.EOF
		}
		a := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return a, nil
	}
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return next() })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return next() })
}

// Radio list that is unavailable like a curses import failure, every menu taking its numbered fallback
func noCursesMenus(t *testing.T) {
	t.Helper()
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) {
		return 0, errors.New("ImportError")
	})
}

// Models probe double returning the models and recording the api key of every call
func recordedFetch(t *testing.T, models []string) *[]string {
	t.Helper()
	keys := &[]string{}
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(_ context.Context, apiKey, _ string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		*keys = append(*keys, apiKey)
		return models, models != nil
	})
	return keys
}

// The custom provider switch fixture home with its config.yaml body
func customSwitchHome(t *testing.T, body string) (string, *ModelFlowRun) {
	t.Helper()
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, body)
	if err := os.WriteFile(home.EnvPath(h), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := pickerCaseRun(t, ctx, h, out)
	r.Config = map[string]any{}
	return h, r
}

func configText(t *testing.T, h string) string {
	t.Helper()
	raw, err := os.ReadFile(home.ConfigPath(h))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCustomProviderModelSwitchCustomEndpointSwitchPrunesStaleModelConfigPoolEntry(t *testing.T) {
	h, r := customSwitchHome(t, "model:\n  default: old-model\n  provider: custom\n  base_url: https://old.example.test/v1\n  api_key: sk-old-model-config\n"+
		"custom_providers:\n- name: Old Endpoint\n  base_url: https://old.example.test/v1\n  api_key: sk-old-config\n  model: old-model\n")
	t.Setenv(config.CustomEndpointKeyEnv("new.example.test"), "")
	store := r.store()
	if _, err := store.WriteCredentialPool("custom:old-endpoint", []map[string]any{
		{"id": "old-model-config", "source": "model_config", "auth_type": "api_key", "access_token": "sk-old-model-config", "base_url": "https://old.example.test/v1", "label": "model_config"},
		{"id": "old-manual", "source": "manual", "auth_type": "api_key", "access_token": "sk-old-manual", "base_url": "https://old.example.test/v1", "label": "manual"},
	}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	swapFlowSeam(t, &customProbeAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ProbeResult {
		return ProbeResult{Models: []string{"new-model"}, Found: true, ProbedURL: "https://new.example.test/v1/models"}
	})
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return "sk-new", nil })
	swapFlowSeam(t, &promptCustomAPIModeSelectionFn, func(*ModelFlowRun, string, string) (string, error) { return "", nil })
	scriptBuiltinsInput(t, "https://new.example.test/v1", "", "", "New Endpoint")
	if err := modelFlowCustom(r); err != nil {
		t.Fatal(err)
	}
	pools, err := store.ReadCredentialPools()
	if err != nil {
		t.Fatal(err)
	}
	var oldSources []string
	for _, e := range pools["custom:old-endpoint"] {
		oldSources = append(oldSources, anyString(e["source"]))
	}
	if strings.Join(oldSources, ",") != "manual" {
		t.Fatalf("old sources %v", oldSources)
	}
	selected, ok := LoadPool(r.Env, "custom:new-endpoint").Select("")
	if !ok || selected.AccessToken != "sk-new" {
		t.Fatalf("new pool %+v %v", selected, ok)
	}
	if model := readConfig(t, h)["model"].(map[string]any); model["base_url"] != "https://new.example.test/v1" {
		t.Fatalf("model %v", model)
	}
}

func TestCustomProviderModelSwitchEnvTemplateApiKeyIsPreservedInModelConfig(t *testing.T) {
	h, r := customSwitchHome(t, "model:\n  default: old-model\n  provider: openrouter\ncustom_providers:\n- name: Example Provider\n"+
		"  base_url: https://api.example-provider.test/v1\n  api_key: ${EXAMPLE_PROVIDER_API_KEY}\n  model: qwen3.6-35b-fast\n")
	t.Setenv("EXAMPLE_PROVIDER_API_KEY", "sk-live-example-provider")
	keys := recordedFetch(t, []string{"qwen3.6-35b-fast"})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "Example Provider", "base_url": "https://api.example-provider.test/v1", "api_key": "sk-live-example-provider",
		"api_key_ref": "${EXAMPLE_PROVIDER_API_KEY}", "model": "qwen3.6-35b-fast"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	if len(*keys) == 0 || (*keys)[len(*keys)-1] != "sk-live-example-provider" {
		t.Fatalf("probe keys %v", *keys)
	}
	cfg := readConfig(t, h)
	if cfg["model"].(map[string]any)["api_key"] != "${EXAMPLE_PROVIDER_API_KEY}" ||
		cfg["custom_providers"].([]any)[0].(map[string]any)["api_key"] != "${EXAMPLE_PROVIDER_API_KEY}" {
		t.Fatalf("config %v", cfg)
	}
	if strings.Contains(configText(t, h), "sk-live-example-provider") {
		t.Fatal("secret written to config.yaml")
	}
}

func TestCustomProviderModelSwitchKeyEnvCustomProviderPersistsReferenceNotSecret(t *testing.T) {
	h, r := customSwitchHome(t, "model:\n  default: old-model\ncustom_providers:\n- name: Example Provider\n"+
		"  base_url: https://api.example-provider.test/v1\n  key_env: EXAMPLE_PROVIDER_API_KEY\n  model: qwen3.6-35b-fast\n")
	t.Setenv("EXAMPLE_PROVIDER_API_KEY", "sk-live-example-provider")
	recordedFetch(t, []string{"qwen3.6-35b-fast"})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "Example Provider", "base_url": "https://api.example-provider.test/v1", "api_key": "",
		"key_env": "EXAMPLE_PROVIDER_API_KEY", "model": "qwen3.6-35b-fast"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	if cfg["model"].(map[string]any)["api_key"] != "${EXAMPLE_PROVIDER_API_KEY}" ||
		cfg["custom_providers"].([]any)[0].(map[string]any)["key_env"] != "EXAMPLE_PROVIDER_API_KEY" {
		t.Fatalf("config %v", cfg)
	}
	if strings.Contains(configText(t, h), "sk-live-example-provider") {
		t.Fatal("secret written to config.yaml")
	}
}

func TestCustomProviderModelSwitchEnvRefBaseUrlPreservesApiKeyRefThroughPicker(t *testing.T) {
	h, r := customSwitchHome(t, "model:\n  default: old-model\n  provider: openrouter\ncustom_providers:\n- name: NeuralWatt\n"+
		"  base_url: ${NEURALWATT_API_BASE}\n  api_key: ${NEURALWATT_API_KEY}\n  model: qwen3.6-35b-fast\n  models: []\n")
	t.Setenv("NEURALWATT_API_BASE", "https://api.neuralwatt.com/v1")
	t.Setenv("NEURALWATT_API_KEY", "sk-live-neuralwatt-secret")
	swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(_ io.Writer, labels []string, def int, title string) (int, bool) {
		for i, label := range labels {
			if strings.Contains(label, "NeuralWatt") {
				return i, true
			}
		}
		t.Fatalf("NeuralWatt entry missing from provider menu: %v", labels)
		return 0, false
	})
	keys := recordedFetch(t, []string{"qwen3.6-35b-fast"})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	if err := SelectProviderAndModel(r.ctx(), nil); err != nil {
		t.Fatal(err)
	}
	if len(*keys) != 1 || (*keys)[0] != "sk-live-neuralwatt-secret" {
		t.Fatalf("probe keys %v", *keys)
	}
	saved := configText(t, h)
	cfg := readConfig(t, h)
	if cfg["model"].(map[string]any)["api_key"] != "${NEURALWATT_API_KEY}" ||
		cfg["custom_providers"].([]any)[0].(map[string]any)["api_key"] != "${NEURALWATT_API_KEY}" {
		t.Fatalf("config %v", cfg)
	}
	if strings.Contains(saved, "sk-live-neuralwatt-secret") {
		t.Fatal("secret written to config.yaml")
	}
}

func TestCustomProviderModelSwitchKeyEnvProvidersDictEntryDoesNotAddApiKey(t *testing.T) {
	h, r := customSwitchHome(t, "providers:\n  crs-henkee:\n    name: CRS Henkee\n    base_url: http://127.0.0.1:3000/api/v1\n"+
		"    key_env: ICEBATH_CRS_HENKEE_KEY\n    transport: anthropic_messages\n    model: claude-opus-4-7\n    default_model: claude-opus-4-7\ncustom_providers: []\n")
	t.Setenv("ICEBATH_CRS_HENKEE_KEY", "cr_live_secret_xyz")
	keys := recordedFetch(t, []string{"claude-opus-4-7"})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "CRS Henkee", "base_url": "http://127.0.0.1:3000/api/v1", "api_key": "", "key_env": "ICEBATH_CRS_HENKEE_KEY",
		"model": "claude-opus-4-7", "api_mode": "anthropic_messages", "provider_key": "crs-henkee", "api_key_ref": ""}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	if len(*keys) != 1 || (*keys)[0] != "cr_live_secret_xyz" {
		t.Fatalf("probe keys %v", *keys)
	}
	savedText := configText(t, h)
	saved := readConfig(t, h)
	entry := saved["providers"].(map[string]any)["crs-henkee"].(map[string]any)
	if saved["model"].(map[string]any)["provider"] != "custom:crs-henkee" {
		t.Fatalf("model %v", saved["model"])
	}
	if _, has := entry["api_key"]; has {
		t.Fatalf("providers.crs-henkee gained an api_key field: %v", entry["api_key"])
	}
	if entry["key_env"] != "ICEBATH_CRS_HENKEE_KEY" || entry["default_model"] != "claude-opus-4-7" {
		t.Fatalf("entry %v", entry)
	}
	if strings.Contains(savedText, "cr_live_secret_xyz") || strings.Contains(savedText, "${ICEBATH_CRS_HENKEE_KEY}") {
		t.Fatalf("config text %q", savedText)
	}
}

func TestCustomProviderModelSwitchPickerRecognizesCurrentProviderAliasWhenNameDiffers(t *testing.T) {
	for _, stored := range []string{"local-127.0.0.1:11434", "custom:local-ollama", "custom:local-127.0.0.1:11434"} {
		t.Run(stored, func(t *testing.T) {
			_, r := customSwitchHome(t, "model:\n  provider: "+stored+"\n  default: qwen3.5:9b\nproviders:\n  local-127.0.0.1:11434:\n"+
				"    name: Local Ollama\n    base_url: http://127.0.0.1:11434/v1\n    default_model: qwen3.5:9b\n    models:\n      qwen3.5:9b: {}\ncustom_providers: []\n")
			var labels []string
			def := -1
			swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(_ io.Writer, choices []string, d int, title string) (int, bool) {
				labels, def = choices, d
				return len(choices) - 1, true
			})
			if err := SelectProviderAndModel(r.ctx(), nil); err != nil {
				t.Fatal(err)
			}
			if def < 0 || !strings.Contains(labels[def], "Local Ollama") || !strings.Contains(labels[def], "currently active") {
				t.Fatalf("default %d labels %v", def, labels)
			}
		})
	}
}

func TestCustomProviderModelSwitchKeyEnvProvidersDictPreservesExistingApiKey(t *testing.T) {
	h, r := customSwitchHome(t, "providers:\n  crs-henkee:\n    name: CRS Henkee\n    base_url: http://127.0.0.1:3000/api/v1\n"+
		"    api_key: ${ICEBATH_CRS_HENKEE_KEY}\n    key_env: ICEBATH_CRS_HENKEE_KEY\n    transport: anthropic_messages\n    model: claude-opus-4-7\n"+
		"    default_model: claude-opus-4-7\ncustom_providers: []\n")
	t.Setenv("ICEBATH_CRS_HENKEE_KEY", "cr_live_secret_xyz")
	recordedFetch(t, []string{"claude-opus-4-7"})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "CRS Henkee", "base_url": "http://127.0.0.1:3000/api/v1", "api_key": "cr_live_secret_xyz", "key_env": "ICEBATH_CRS_HENKEE_KEY",
		"model": "claude-opus-4-7", "api_mode": "anthropic_messages", "provider_key": "crs-henkee", "api_key_ref": "${ICEBATH_CRS_HENKEE_KEY}"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	entry := readConfig(t, h)["providers"].(map[string]any)["crs-henkee"].(map[string]any)
	if entry["api_key"] != "${ICEBATH_CRS_HENKEE_KEY}" || strings.Contains(configText(t, h), "cr_live_secret_xyz") {
		t.Fatalf("entry %v", entry)
	}
}

// Counts the native Ollama catalog probes of the named custom flow
func countOllamaProbes(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	swapFlowSeam(t, &fetchOllamaLocalModelsFn, func(context.Context, string, time.Duration, map[string]string) ([]string, bool) {
		*calls++
		return nil, false
	})
	return calls
}

// Ordered models mapping like the hermes dict literal
func orderedModels(ids ...string) map[string]any {
	m := map[string]any{}
	for _, id := range ids {
		m[id] = map[string]any{}
	}
	config.SetMappingKeys(m, ids)
	return m
}

func TestCustomProviderDiscoverModelsDiscoverFalseWithOnlySingularModelSkipsProbe(t *testing.T) {
	_, r := customSwitchHome(t, "custom_providers: []\nmodel: old-model\n")
	keys := recordedFetch(t, nil)
	ollama := countOllamaProbes(t)
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "Headered Ollama", "base_url": "http://127.0.0.1:11434", "api_key": "no-key-required", "discover_models": false, "model": "qwen3:8b"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	if len(*keys) != 0 || *ollama != 0 {
		t.Fatalf("fetch %v ollama %d", *keys, *ollama)
	}
}

func TestCustomProviderDiscoverModelsDiscoverFalseSavesChoiceFromConfiguredList(t *testing.T) {
	h, r := customSwitchHome(t, "custom_providers: []\nmodel: old-model\n")
	keys := recordedFetch(t, nil)
	noCursesMenus(t)
	scriptBuiltinsInput(t, "2")
	info := map[string]any{"name": "Baidu Coding", "base_url": "https://qianfan.baidubce.com/v2/coding", "api_key": "sk-test",
		"discover_models": false, "models": orderedModels("kimi-k2.5", "glm-5"), "model": "kimi-k2.5"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	model, ok := readConfig(t, h)["model"].(map[string]any)
	if len(*keys) != 0 || !ok || model["default"] != "glm-5" {
		t.Fatalf("fetch %v model %v", *keys, readConfig(t, h)["model"])
	}
}

func TestCustomProviderDiscoverModelsProbeEmptyFallsBackToConfiguredList(t *testing.T) {
	h, r := customSwitchHome(t, "custom_providers: []\nmodel: old-model\n")
	recordedFetch(t, []string{})
	noCursesMenus(t)
	scriptBuiltinsInput(t, "2")
	info := map[string]any{"name": "My Gateway", "base_url": "https://gw.example.com/v1", "api_key": "sk-test",
		"models": orderedModels("fallback-a", "fallback-b"), "model": "fallback-a"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	if model, ok := readConfig(t, h)["model"].(map[string]any); !ok || model["default"] != "fallback-b" {
		t.Fatalf("model %v", readConfig(t, h)["model"])
	}
}

func TestCustomProviderDiscoverModelsDiscoverFalseStringIsNormalised(t *testing.T) {
	_, r := customSwitchHome(t, "custom_providers: []\nmodel: old-model\n")
	keys := recordedFetch(t, nil)
	noCursesMenus(t)
	scriptBuiltinsInput(t, "1")
	info := map[string]any{"name": "Baidu Coding", "base_url": "https://qianfan.baidubce.com/v2/coding", "api_key": "sk-test",
		"discover_models": "false", "models": orderedModels("kimi-k2.5", "glm-5"), "model": "kimi-k2.5"}
	if err := modelFlowNamedCustom(r, info); err != nil {
		t.Fatal(err)
	}
	if len(*keys) != 0 {
		t.Fatalf("fetch %v", *keys)
	}
}
