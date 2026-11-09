package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Replaces a flow seam for one test and restores it afterwards
func swapFlowSeam[T any](t *testing.T, p *T, v T) {
	t.Helper()
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// A flow run over the home with its output captured
func newFlowRun(t *testing.T, h string) (*ModelFlowRun, *bytes.Buffer) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), h)
	env := testEnv(t, h)
	env.Ctx = ctx
	out := &bytes.Buffer{}
	return &ModelFlowRun{Ctx: ctx, Env: env, Out: out, Config: map[string]any{}}, out
}

func TestXaiModelFlowReauthUsesStandardRadioPrompt(t *testing.T) {
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	calls := 0
	var gotArgs XAILoginArgs
	gotForce := false
	swapFlowSeam(t, &flowXAIStatusFn, func(PoolEnv) map[string]any { return map[string]any{"logged_in": true} })
	swapFlowSeam(t, &flowCursesChoiceFn, func(string, []string, int, string) (int, error) { return 1, nil })
	swapFlowSeam(t, &flowLoginXAIFn, func(_ PoolEnv, _ io.Writer, args XAILoginArgs, force bool) error {
		calls++
		gotArgs, gotForce = args, force
		return nil
	})
	swapFlowSeam(t, &flowResolveXAIFn, func(PoolEnv, bool, bool, time.Duration) (map[string]any, error) {
		return map[string]any{"base_url": "https://api.x.ai/v1"}, nil
	})
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "", false })
	r.CurrentModel = "grok-build-0.1"
	r.Args = map[string]any{"no_browser": true, "timeout": 3}
	if err := modelFlowXAIOAuth(r); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !gotForce || !gotArgs.NoBrowser || gotArgs.Timeout != 3*time.Second {
		t.Fatalf("calls %d force %v args %+v", calls, gotForce, gotArgs)
	}
}

func TestXaiModelFlowCancelSkipsReauth(t *testing.T) {
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	swapFlowSeam(t, &flowXAIStatusFn, func(PoolEnv) map[string]any { return map[string]any{"logged_in": true} })
	swapFlowSeam(t, &flowCursesChoiceFn, func(string, []string, int, string) (int, error) { return 2, nil })
	swapFlowSeam(t, &flowLoginXAIFn, func(PoolEnv, io.Writer, XAILoginArgs, bool) error {
		t.Fatal("should not reauthenticate")
		return nil
	})
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) {
		t.Fatal("should not pick a model")
		return "", false
	})
	r.CurrentModel = "grok-build-0.1"
	if err := modelFlowXAIOAuth(r); err != nil {
		t.Fatal(err)
	}
}

func TestAuthCredentialsChoiceFallsBackToNumberedPrompt(t *testing.T) {
	swapFlowSeam(t, &flowCursesChoiceFn, func(string, []string, int, string) (int, error) { return -1, nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "2", nil })
	if got := promptAuthCredentialsChoice(io.Discard, "Credentials:"); got != "reauth" {
		t.Fatalf("choice %q", got)
	}
}

// Silences every flow seam that reaches the network or a terminal for the api key flow tests
func silenceAPIKeyFlowSeams(t *testing.T) {
	t.Helper()
	swapFlowSeam(t, &envBaseURLFn, func(*ModelFlowRun, string) string { return "" })
	swapFlowSeam(t, &promptBaseURLOverrideFn, func(_ *ModelFlowRun, value, _ string, _ bool) (string, error) { return value, nil })
	swapFlowSeam(t, &modelsDevMergedFn, func(context.Context, string, []string) []string { return []string{} })
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{}, false
	})
	swapFlowSeam(t, &flowPricingFn, func(context.Context, string, string, bool, bool) PricingTable { return nil })
}

func TestSetupOffersRegisteredProviderCatalog(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live_%v", live), func(t *testing.T) {
			h := newTestHome(t)
			r, _ := newFlowRun(t, h)
			profile := Profile{Name: "scout-setup-catalog", DisplayName: "Setup catalog", AuthType: AuthTypeAPIKey, KeyEnv: []string{"SCOUT_SETUP_TEST_KEY"},
				BaseURL: "https://setup.example.invalid/v1", FallbackModels: []string{"declared-plugin-model"}}
			profile.FetchModelsHook = func(_ context.Context, apiKey, baseURL string, _ time.Duration) ([]string, bool) {
				if apiKey != "synthetic-test-key" || baseURL != "https://setup.example.invalid/v1" {
					t.Errorf("fetch_models got %q %q", apiKey, baseURL)
				}
				if live {
					return []string{"declared-plugin-model"}, true
				}
				return nil, false
			}
			registerTestProfile(t, profile)
			silenceAPIKeyFlowSeams(t)
			swapFlowSeam(t, &ensureFlowAPIKeyFn, func(PoolEnv, io.Writer, string, Profile, []string) (string, string, bool, error) {
				return "", "synthetic-test-key", false, nil
			})
			var picked []string
			swapFlowSeam(t, &pickModelOrPromptFn, func(_ PoolEnv, _ io.Writer, list []string, _ string, _ ModelSelectionPrompt) (string, bool) {
				picked = list
				return "", false
			})
			swapFlowSeam(t, &finishModelFn, func(PoolEnv, io.Writer, string, string, string, string, persistModelOptions) (map[string]any, error) {
				return nil, nil
			})
			if err := modelFlowAPIKeyProvider(r, profile.Name); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(picked, []string{"declared-plugin-model"}) {
				t.Fatalf("picker rows %q", picked)
			}
		})
	}
}

func TestSetupUsesProfileEndpointAndHeaders(t *testing.T) {
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	type request struct{ path, contract string }
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests = append(requests, request{req.URL.Path, req.Header.Get("X-Catalog-Contract")})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "live-catalog-model"}]}`))
	}))
	defer server.Close()
	profile := Profile{Name: "scout-live-catalog", BaseURL: server.URL + "/inference", ModelsURL: server.URL + "/catalog", ModelListing: true,
		Headers: map[string]string{"X-Catalog-Contract": "present"}}
	registerTestProfile(t, profile)
	swapFlowSeam(t, &modelsDevMergedFn, func(context.Context, string, []string) []string { return []string{} })
	result, err := apiKeyProviderModelList(r, profile.Name, Profile{DisplayName: "Test catalog"}, "synthetic-test-key", "", profile.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, []string{"live-catalog-model"}) {
		t.Fatalf("rows %q", result)
	}
	if !reflect.DeepEqual(requests, []request{{"/catalog", "present"}}) {
		t.Fatalf("requests %+v", requests)
	}
}

func TestSetupMatchesPickerWhenCatalogFetchFails(t *testing.T) {
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	profile := Profile{Name: "scout-down-catalog", AuthType: AuthTypeAPIKey, KeyEnv: []string{"SCOUT_DOWN_TEST_KEY"},
		BaseURL: "https://down.example.invalid/v1", FallbackModels: []string{"declared-a", "declared-b"}}
	profile.FetchModelsHook = func(context.Context, string, string, time.Duration) ([]string, bool) { return nil, false }
	registerTestProfile(t, profile)
	swapFlowSeam(t, &modelsDevMergedFn, func(context.Context, string, []string) []string { return []string{} })
	swapFlowSeam(t, &apiKeyCredentialsFn, func(context.Context, string) (string, string) { return "synthetic-test-key", "" })
	setupRows, err := apiKeyProviderModelList(r, profile.Name, Profile{DisplayName: "Down"}, "synthetic-test-key", "", profile.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	pickerRows := ProviderModelIDs(r.ctx(), profile.Name, false).IDs
	if !reflect.DeepEqual(setupRows, []string{"declared-a", "declared-b"}) || !reflect.DeepEqual(pickerRows, setupRows) {
		t.Fatalf("setup %q picker %q", setupRows, pickerRows)
	}
}

func TestSwitchValidationTrustsProfileOwnedCatalog(t *testing.T) {
	h := newTestHome(t)
	ctx := home.WithOverride(context.Background(), h)
	profile := Profile{Name: "scout-plan", AuthType: AuthTypeAPIKey, KeyEnv: []string{"SCOUT_PLAN_TEST_KEY"}, BaseURL: "https://plan.example.invalid/v1"}
	profile.FetchModelsHook = func(context.Context, string, string, time.Duration) ([]string, bool) {
		return []string{"plan/model-1"}, true
	}
	registerTestProfile(t, profile)
	swapFlowSeam(t, &apiKeyCredentialsFn, func(context.Context, string) (string, string) { return "synthetic-test-key", "" })
	swapFlowSeam(t, &fetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"other-vendor/model-a"}, true
	})
	opts := ValidateModelOptions{APIKey: "synthetic-test-key", BaseURL: profile.BaseURL}
	if !ValidateRequestedModel(ctx, "plan/model-1", profile.Name, opts).Accepted {
		t.Fatal("the profile owned catalog model was rejected")
	}
	if ValidateRequestedModel(ctx, "plan/model-9", profile.Name, opts).Accepted {
		t.Fatal("a model in neither catalog was accepted")
	}
}

// Installs a curated row for the provider the way the hermes tests patch _PROVIDER_MODELS
func setCuratedModels(t *testing.T, provider string, ids []string) {
	t.Helper()
	HasCuratedCatalog(provider)
	providerModelsMu.Lock()
	prev, had := providerModels[provider]
	providerModels[provider] = ids
	providerModelsMu.Unlock()
	t.Cleanup(func() {
		providerModelsMu.Lock()
		defer providerModelsMu.Unlock()
		if had {
			providerModels[provider] = prev
		} else {
			delete(providerModels, provider)
		}
	})
}

func TestSetupKeepsCuratedListWhenProfileCatalogIsDownAndDeclaresNoFallback(t *testing.T) {
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	profile := Profile{Name: "scout-curated-only", AuthType: AuthTypeAPIKey, KeyEnv: []string{"SCOUT_CURATED_KEY"}, BaseURL: "https://down.example.invalid/v1"}
	profile.FetchModelsHook = func(context.Context, string, string, time.Duration) ([]string, bool) { return nil, false }
	registerTestProfile(t, profile)
	setCuratedModels(t, profile.Name, []string{"curated-a", "curated-b"})
	swapFlowSeam(t, &modelsDevMergedFn, func(context.Context, string, []string) []string { return []string{} })
	setupRows, err := apiKeyProviderModelList(r, profile.Name, Profile{DisplayName: "Down"}, "synthetic-test-key", "", profile.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(setupRows, []string{"curated-a", "curated-b"}) {
		t.Fatalf("rows %q", setupRows)
	}
}

// Isolated home with an empty config and no ambient provider env, the config_home fixture
func geminiConfigHome(t *testing.T) string {
	t.Helper()
	h := newTestHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model: some-old-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ICEBATH_MODEL", "LLM_MODEL", "ICEBATH_INFERENCE_PROVIDER", "OPENAI_BASE_URL", "OPENAI_API_KEY", "GEMINI_BASE_URL"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	config.ResetCache()
	return h
}

// Runs the api key flow for the provider with the prompts answered by Enter and the picker scripted
func runGeminiBlockFlow(t *testing.T, h, providerID, pick string) string {
	t.Helper()
	r, out := newFlowRun(t, h)
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return pick, true })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "", nil })
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return "", nil })
	swapFlowSeam(t, &flowListAgenticFn, func(context.Context, string, bool) []string { return nil })
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	r.Config = cfg
	r.CurrentModel = "old-model"
	if err := modelFlowAPIKeyProvider(r, providerID); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The model section of the home's config.yaml as saved on disk
func savedModelSection(t *testing.T, h string) (map[string]any, any) {
	t.Helper()
	config.ResetCache()
	doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := doc.(map[string]any)
	model, _ := cfg["model"].(map[string]any)
	return model, cfg["model"]
}

func TestGeminiSetupFreeTierBlockFreeTierKeyIsBlocked(t *testing.T) {
	h := geminiConfigHome(t)
	t.Setenv("GOOGLE_API_KEY", "fake-free-tier-key")
	swapFlowSeam(t, &probeGeminiTierFn, func(context.Context, string, string, string, time.Duration) string { return "free" })
	out := runGeminiBlockFlow(t, h, "gemini", "gemini-2.5-flash")
	if !strings.Contains(strings.ToLower(out), "free tier") || !strings.Contains(out, "aistudio.google.com/apikey") || !strings.Contains(out, "Not saving Gemini as the default provider") {
		t.Fatalf("output %q", out)
	}
	if model, _ := savedModelSection(t, h); model != nil && model["provider"] == "gemini" {
		t.Fatalf("Free-tier key should not have saved gemini as provider: %v", model)
	}
}

func TestGeminiSetupFreeTierBlockPaidTierKeyProceeds(t *testing.T) {
	h := geminiConfigHome(t)
	t.Setenv("GOOGLE_API_KEY", "fake-paid-tier-key")
	swapFlowSeam(t, &probeGeminiTierFn, func(context.Context, string, string, string, time.Duration) string { return "paid" })
	out := runGeminiBlockFlow(t, h, "gemini", "gemini-2.5-flash")
	if !strings.Contains(strings.ToLower(out), "paid") || strings.Contains(out, "Not saving Gemini") {
		t.Fatalf("output %q", out)
	}
	model, raw := savedModelSection(t, h)
	if model == nil {
		t.Fatalf("model should be dict, got %T", raw)
	}
	if model["provider"] != "gemini" || model["default"] != "gemini-2.5-flash" {
		t.Fatalf("model %v", model)
	}
}

func TestGeminiSetupFreeTierBlockNonGeminiProviderSkipsProbe(t *testing.T) {
	h := geminiConfigHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "fake-key")
	swapFlowSeam(t, &probeGeminiTierFn, func(context.Context, string, string, string, time.Duration) string {
		t.Fatal("probe_gemini_tier must only run for gemini")
		return ""
	})
	runGeminiBlockFlow(t, h, "deepseek", "deepseek-chat")
}

// External process profile whose setup status and discovered rows the test scripts
func procProviderProfile(t *testing.T, status map[string]any, live []map[string]any) Profile {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "proc-provider", DisplayName: "Proc Provider", AuthType: AuthTypeExternalProcess, BaseURL: "process://proc-provider",
		ProcessCommand: exe, FallbackModels: []string{"pinned-a", "pinned-b"}}
	p.SetupStatusHook = func(context.Context, map[string]any) (map[string]any, error) { return status, nil }
	p.DiscoverModelsHook = func(context.Context, map[string]any) ([]map[string]any, error) { return live, nil }
	return p
}

// Drives the generic plugin flow for the profile and reports what the picker and persist step saw
func runProcProviderFlow(t *testing.T, profile Profile) (map[string]any, string) {
	t.Helper()
	registerTestProfile(t, profile)
	h := newTestHome(t)
	r, out := newFlowRun(t, h)
	picked := map[string]any{}
	swapFlowSeam(t, &pickModelOrPromptFn, func(_ PoolEnv, _ io.Writer, list []string, _ string, p ModelSelectionPrompt) (string, bool) {
		picked["models"] = list
		picked["notes"] = p.Notes
		if len(list) > 0 {
			return list[0], true
		}
		return "", false
	})
	swapFlowSeam(t, &finishModelFn, func(_ PoolEnv, _ io.Writer, selected, _, _, _ string, _ persistModelOptions) (map[string]any, error) {
		picked["finished"] = selected
		return nil, nil
	})
	if err := modelFlowPluginProvider(r, profile.Name); err != nil {
		t.Fatal(err)
	}
	return picked, out.String()
}

func TestLoggedOutWithoutTtyStopsWithLoginInstruction(t *testing.T) {
	status := map[string]any{"available": true, "logged_in": false, "plan": "", "login_command": []string{"claude", "auth", "login"},
		"detail": "Claude Code is installed but not logged in. Run `claude auth login`, then select this provider again."}
	swapFlowSeam(t, &flowStdinIsTTYFn, func() bool { return false })
	picked, out := runProcProviderFlow(t, procProviderProfile(t, status, nil))
	if len(picked) != 0 || !strings.Contains(out, "not logged in") || !strings.Contains(out, "claude auth login") {
		t.Fatalf("picked %v output %q", picked, out)
	}
}

func TestLoggedInListsLivePickerWithNotes(t *testing.T) {
	status := map[string]any{"available": true, "logged_in": true, "plan": "Claude Pro", "login_command": []string{"claude", "auth", "login"}, "detail": ""}
	live := []map[string]any{{"id": "claude-sonnet-5[1m]", "label": "Sonnet 5", "note": ""},
		{"id": "claude-fable-5-1[1m]", "label": "Fable 5.1", "note": "usage credits"}}
	picked, out := runProcProviderFlow(t, procProviderProfile(t, status, live))
	if !strings.Contains(out, "Claude Pro") {
		t.Fatalf("output %q", out)
	}
	models, _ := picked["models"].([]string)
	have := map[string]bool{}
	for _, m := range models {
		have[m] = true
	}
	for _, want := range []string{"claude-sonnet-5[1m]", "claude-fable-5-1[1m]", "pinned-a", "pinned-b"} {
		if !have[want] {
			t.Fatalf("models %q lack %q", models, want)
		}
	}
	if !reflect.DeepEqual(picked["notes"], map[string]string{"claude-fable-5-1[1m]": "usage credits"}) {
		t.Fatalf("notes %v", picked["notes"])
	}
	if picked["finished"] != models[0] {
		t.Fatalf("finished %v models %q", picked["finished"], models)
	}
}

func TestMissingCliStopsBeforeAnyPick(t *testing.T) {
	detail := "Claude Code is not installed (no `claude` on PATH). Install it with `npm install -g @anthropic-ai/claude-code`."
	for _, viaStatus := range []bool{true, false} {
		t.Run(fmt.Sprintf("via_status_%v", viaStatus), func(t *testing.T) {
			status := map[string]any{"available": false, "logged_in": false, "plan": "", "login_command": nil, "detail": detail}
			var profile Profile
			if viaStatus {
				profile = procProviderProfile(t, status, nil)
			} else {
				profile = procProviderProfile(t, nil, nil)
				profile.ProcessCommand = "definitely-not-a-real-cli-binary"
			}
			picked, out := runProcProviderFlow(t, profile)
			if len(picked) != 0 {
				t.Fatalf("picked %v", picked)
			}
			if viaStatus && !strings.Contains(out, detail) {
				t.Fatalf("output %q", out)
			}
			if !viaStatus && (!strings.Contains(out, "Could not find") || !strings.Contains(out, "definitely-not-a-real-cli-binary")) {
				t.Fatalf("output %q", out)
			}
		})
	}
}

func TestGenericApiKeyFlowPassesPoolKeyToExistingKeyPrompt(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	os.Unsetenv("DEEPSEEK_API_KEY")
	r, _ := newFlowRun(t, h)
	if _, err := r.store().WriteCredentialPool("deepseek", []map[string]any{{"id": "pool-1", "label": "pool", "auth_type": "api_key", "priority": 0,
		"source": "manual", "access_token": "pool-secret"}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	swapFlowSeam(t, &flowGetEnvValueFn, func(context.Context, string, string) (string, bool, error) { return "", false, nil })
	captured := map[string]string{}
	swapFlowSeam(t, &promptAPIKeyFn, func(_ PoolEnv, _ io.Writer, _ Profile, existingKey, _, _ string) (string, bool, error) {
		captured["existing_key"] = existingKey
		return existingKey, true, nil
	})
	if err := modelFlowAPIKeyProvider(r, "deepseek"); err != nil {
		t.Fatal(err)
	}
	if captured["existing_key"] != "pool-secret" {
		t.Fatalf("existing key %q", captured["existing_key"])
	}
}

func TestActualSetupKeepsProviderSettingsInYaml(t *testing.T) {
	for _, configuredProvider := range []string{"actual", "aci"} {
		for _, override := range []string{"", "http://127.0.0.1:8081", "invalid-url"} {
			t.Run(configuredProvider+"_"+override, func(t *testing.T) {
				h := newTestHome(t)
				t.Setenv("ACTUAL_API_KEY", "actual-test-key")
				t.Setenv("ACTUAL_BASE_URL", "http://127.0.0.1:8089")
				configuredURL := "http://127.0.0.1:8080"
				configPath := filepath.Join(h, "config.yaml")
				if err := os.WriteFile(configPath, []byte(fmt.Sprintf("model:\n  base_url: %s\n  default: old-model\n  provider: %s\n", configuredURL, configuredProvider)), 0o600); err != nil {
					t.Fatal(err)
				}
				envPath := filepath.Join(h, ".env")
				if err := os.WriteFile(envPath, []byte("ACTUAL_API_KEY=actual-test-key\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				config.ResetCache()
				swapFlowSeam(t, &ensureFlowAPIKeyFn, func(PoolEnv, io.Writer, string, Profile, []string) (string, string, bool, error) {
					return "actual-test-key", "actual-test-key", false, nil
				})
				swapFlowSeam(t, &flowAskFn, func(io.Writer, string, flowAskOptions) (string, bool) { return override, true })
				swapFlowSeam(t, &apiKeyProviderModelListFn, func(*ModelFlowRun, string, Profile, string, string, string) ([]string, error) { return []string{}, nil })
				swapFlowSeam(t, &pickModelOrPromptFn, func(PoolEnv, io.Writer, []string, string, ModelSelectionPrompt) (string, bool) {
					return "test-model", true
				})
				r, _ := newFlowRun(t, h)
				cfg, err := r.loadConfig()
				if err != nil {
					t.Fatal(err)
				}
				r.Config = cfg
				if err := modelFlowAPIKeyProvider(r, "actual"); err != nil {
					t.Fatal(err)
				}
				expected := configuredURL
				if strings.HasPrefix(override, "http://") {
					expected = override
				}
				saved, _ := savedModelSection(t, h)
				if saved["provider"] != "actual" || saved["base_url"] != expected || saved["default"] != "test-model" {
					t.Fatalf("saved %v", saved)
				}
				envText, err := os.ReadFile(envPath)
				if err != nil || string(envText) != "ACTUAL_API_KEY=actual-test-key\n" {
					t.Fatalf(".env %q %v", envText, err)
				}
				if p := getProviderProfile(r.ctx(), "actual"); p == nil || !reflect.DeepEqual(p.KeyEnv, []string{"ACTUAL_API_KEY"}) {
					t.Fatalf("actual profile %+v", p)
				}
				for _, name := range []string{"ACTUAL_BASE_URL", "ACTUAL_API_MODE"} {
					if _, held := config.OptionalEnvVars[name]; held {
						t.Fatalf("%s is an optional env var", name)
					}
				}
				creds, err := authResolver(r.Env).apiKeyProviderCredentials("actual")
				if err != nil || creds.BaseURL != expected+"/v1" {
					t.Fatalf("credentials %+v %v", creds, err)
				}
				for _, explicitKey := range []string{"", "actual-test-key"} {
					runtime, err := ResolveRuntimeProvider(r.ctx(), h, "actual", "", "", explicitKey)
					if err != nil || runtime.BaseURL != expected+"/v1" || runtime.APIMode != "chat_completions" {
						t.Fatalf("runtime %+v %v", runtime, err)
					}
				}
			})
		}
	}
}
