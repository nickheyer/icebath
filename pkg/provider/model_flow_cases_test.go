package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Answers line_input, builtin input and the masked prompt from one queue like a patched iterator
func scriptAllInput(t *testing.T, answers ...string) {
	t.Helper()
	next := func() (string, error) {
		if len(answers) == 0 {
			return "", io.EOF
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return next() })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return next() })
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return next() })
}

func TestUpgradeHermesModelSignsAConnectorFreeTierInInsteadOfRefusing(t *testing.T) {
	_, env := installAnonPortal(t)
	desktop := anonDesktopEnv(env)
	state, err := EnsurePortalIdentity(desktop, PortalIdentityOptions{Explicit: true})
	if err != nil || !guestWithoutFreeTierIn(desktop, state) {
		t.Fatalf("guest without free tier %v %v", state, err)
	}
	logins := 0
	swapFlowSeam(t, &flowRunLoginFn, func(io.Writer, *Profile, func() error) bool {
		logins++
		return true
	})
	var out bytes.Buffer
	if err := modelFlowNous(&ModelFlowRun{Ctx: desktop.ctx(), Env: desktop, Out: &out, Config: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if logins != 1 || strings.Contains(out.String(), UpgradeHint) {
		t.Fatalf("logins %d out %q", logins, out.String())
	}
}

func TestStaleOAuthTokenDetectionStaleOauthTokenTriggersReauth(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	for k, v := range map[string]string{"ANTHROPIC_TOKEN": "sk-ant-oat-ExpiredToken00000", "ANTHROPIC_API_KEY": ""} {
		if err := config.SaveEnvValue(h, k, v); err != nil {
			t.Fatal(err)
		}
	}
	swapFlowSeam(t, &anthReadClaudeCodeCredentialsFn, func(PoolEnv) map[string]any {
		return map[string]any{"accessToken": "expired-cc-token", "refreshToken": "", "expiresAt": 0, "source": "claude_code_credentials_file"}
	})
	swapFlowSeam(t, &anthIsClaudeCodeTokenValidFn, func(map[string]any) bool { return false })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "3", nil })
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return "", nil })
	if err := modelFlowAnthropic(pickerCaseRun(t, ctx, h, out)); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "subscription") && !strings.Contains(text, "API key") {
		t.Fatalf("Expected auth method menu but got: %q", text)
	}
}

func TestModelFlowQwenOauthStaleTokenShowsReauthGuidance(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writeQwenCreds(t, map[string]any{"access_token": "dead-at", "refresh_token": "test-refresh-token", "token_type": "Bearer",
		"expiry_date": time.Now().Add(-time.Hour).UnixMilli(), "resource_url": "portal.qwen.ai"})
	swapFlowSeam(t, &refreshQwenCLITokensFn, func(context.Context, map[string]any, time.Duration) (map[string]any, error) {
		return nil, &AuthError{Message: "Qwen refresh rejected. Re-run 'qwen auth qwen-oauth'.", Provider: "qwen-oauth", Code: "qwen_refresh_failed"}
	})
	promptCalled, updateCalled := false, false
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) {
		promptCalled = true
		return "", false
	})
	swapFlowSeam(t, &flowUpdateConfigForProviderFn, func(PoolEnv, string, string, string, bool) (string, error) {
		updateCalled = true
		return "", nil
	})
	r := pickerCaseRun(t, ctx, h, out)
	r.Config, r.CurrentModel = map[string]any{}, "qwen3-coder-plus"
	if err := modelFlowQwenOAuth(r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Qwen refresh rejected") || promptCalled || updateCalled {
		t.Fatalf("out %q prompt %v update %v", out.String(), promptCalled, updateCalled)
	}
}

func TestModelFlowNousDoesNotRestoreStaleCustomApiKey(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "model:\n  provider: custom\n  default: glm-5.2\n  base_url: https://api.neuralwatt.com/v1\n  api_key: ${NEURALWATT_API_KEY}\n  api_mode: chat_completions\n")
	raw, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	selected := "deepseek/deepseek-v4-flash"
	swapFlowSeam(t, &flowNousAuthStateFn, func(*ModelFlowRun) (map[string]any, error) {
		return map[string]any{"access_token": "nous-token", "portal_base_url": "https://portal.example.com"}, nil
	})
	swapFlowSeam(t, &flowResolveNousCredsFn, func(PoolEnv, bool, string) (map[string]any, error) {
		return map[string]any{"base_url": "https://inference-api.nousresearch.com/v1", "api_key": "nous-key"}, nil
	})
	swapFlowSeam(t, &flowCuratedNousIDsFn, func(context.Context) []string { return []string{selected} })
	swapFlowSeam(t, &flowPricingFn, func(context.Context, string, string, bool, bool) PricingTable { return PricingTable{} })
	swapFlowSeam(t, &checkNousFreeTierFn, func(context.Context, bool, bool) bool { return false })
	swapFlowSeam(t, &unionPortalPaidFn, func(_ context.Context, ids []string, pricing PricingTable, _ string, _ bool) ([]string, PricingTable) {
		return ids, pricing
	})
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return selected, true })
	swapFlowSeam(t, &flowEnableToolGatewayFn, func(PoolEnv, io.Writer, map[string]any, bool) (map[string]bool, error) { return nil, nil })
	r := pickerCaseRun(t, ctx, h, out)
	r.Config, r.CurrentModel = raw, "glm-5.2"
	if err := modelFlowNous(r); err != nil {
		t.Fatal(err)
	}
	model := readConfig(t, h)["model"].(map[string]any)
	if model["provider"] != "nous" || model["default"] != selected || model["base_url"] != "https://inference-api.nousresearch.com/v1" {
		t.Fatalf("model %v", model)
	}
	if _, has := model["api_key"]; has {
		t.Fatalf("api_key kept %v", model)
	}
	if _, has := model["api_mode"]; has {
		t.Fatalf("api_mode kept %v", model)
	}
}

// Custom endpoint fixture home whose config model is the empty custom section the hermes cases load
func customFlowHome(t *testing.T) (string, *ModelFlowRun) {
	t.Helper()
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "model:\n  default: ''\n  provider: custom\n  base_url: ''\n")
	if err := os.WriteFile(home.EnvPath(h), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return h, pickerCaseRun(t, ctx, h, out)
}

func TestModelFlowCustomSavesVerifiedV1BaseUrl(t *testing.T) {
	h, r := customFlowHome(t)
	t.Setenv(config.CustomEndpointKeyEnv("localhost_8000"), "")
	var savedModel []string
	swapFlowSeam(t, &flowSaveModelChoiceFn, func(_ PoolEnv, model string) error {
		savedModel = append(savedModel, model)
		return nil
	})
	swapFlowSeam(t, &saveCustomProviderFn, func(*ModelFlowRun, string, string, string, int, string, string, string) error { return nil })
	swapFlowSeam(t, &customProbeAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ProbeResult {
		return ProbeResult{Models: []string{"llm"}, Found: true, ProbedURL: "http://localhost:8000/v1/models",
			ResolvedBaseURL: "http://localhost:8000/v1", SuggestedBaseURL: "http://localhost:8000/v1", UsedFallback: true}
	})
	scriptAllInput(t, "http://localhost:8000", "local-key", "", "", "", "", "")
	callerCfg := map[string]any{}
	r.Config = callerCfg
	if err := modelFlowCustom(r); err != nil {
		t.Fatal(err)
	}
	if model, ok := callerCfg["model"].(map[string]any); !ok || model["base_url"] != "http://localhost:8000/v1" {
		t.Fatalf("caller model %v", callerCfg["model"])
	}
	if _, has := config.LoadEnv(h)["OPENAI_BASE_URL"]; has {
		t.Fatal("OPENAI_BASE_URL saved to .env")
	}
	if len(savedModel) != 1 || savedModel[0] != "llm" {
		t.Fatalf("saved model %v", savedModel)
	}
}

func TestModelFlowCustomPersistsSelectedApiMode(t *testing.T) {
	h, r := customFlowHome(t)
	t.Setenv(config.CustomEndpointKeyEnv("codex.example.com"), "")
	type savedProvider struct {
		baseURL, apiKey, model, name, apiMode, keyEnv string
		contextLength                                 int
	}
	var captured *savedProvider
	swapFlowSeam(t, &saveCustomProviderFn, func(_ *ModelFlowRun, baseURL, apiKey, model string, contextLength int, name, apiMode, keyEnv string) error {
		captured = &savedProvider{baseURL, apiKey, model, name, apiMode, keyEnv, contextLength}
		return nil
	})
	swapFlowSeam(t, &flowSaveModelChoiceFn, func(PoolEnv, string) error { return nil })
	swapFlowSeam(t, &customProbeAPIModelsFn, func(_ context.Context, _ string, baseURL string, _ time.Duration, _ string, _ map[string]string) ProbeResult {
		return ProbeResult{Models: []string{}, Found: true, ProbedURL: strings.TrimRight(baseURL, "/") + "/models"}
	})
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return "test-key", nil })
	scriptBuiltinsInput(t, "https://codex.example.com/v1", "3", "chosen-model", "", "")
	r.Config = map[string]any{"model": map[string]any{"provider": "custom"}}
	if err := modelFlowCustom(r); err != nil {
		t.Fatal(err)
	}
	model := readConfig(t, h)["model"].(map[string]any)
	if model["provider"] != "custom" || model["base_url"] != "https://codex.example.com/v1" || model["api_mode"] != "codex_responses" {
		t.Fatalf("model %v", model)
	}
	if captured == nil || captured.apiMode != "codex_responses" {
		t.Fatalf("captured %+v", captured)
	}
	if model["api_key"] != "${"+captured.keyEnv+"}" || config.LoadEnv(h)[captured.keyEnv] != "test-key" {
		t.Fatalf("model api_key %v env %q", model["api_key"], config.LoadEnv(h)[captured.keyEnv])
	}
}

// Records the request URL and Authorization header of every catalog request, the hermes picker_http fixture
type pickerHTTPRecorder struct {
	mu   sync.Mutex
	seen [][2]string
}

func (p *pickerHTTPRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.seen = append(p.seen, [2]string{req.URL.String(), req.Header.Get("Authorization")})
	p.mu.Unlock()
	body := `{"models":[{"slug":"gpt-5.5","visibility":"list","priority":1,"context_window":272000}]}`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (p *pickerHTTPRecorder) authorizedHosts() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for _, s := range p.seen {
		if s[1] != "" {
			if u, err := url.Parse(s[0]); err == nil {
				out[u.Hostname()] = true
			}
		}
	}
	return out
}

func TestModelSetupFlowCatalogGoesToThePoolEntryGateway(t *testing.T) {
	const gw, chatgpt, opaque = "https://codex-gw.example/backend-api/codex", "https://chatgpt.com/backend-api/codex", "dummy-gateway-pool-key"
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("ICEBATH_CODEX_BASE_URL", "")
	writePickerConfig(t, h, "model:\n  base_url: "+gw+"\n  default: gpt-5.5\n  provider: openai-codex\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{"openai-codex": []any{
		map[string]any{"id": "gw", "label": "gateway", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": opaque, "base_url": chatgpt}}}})
	recorder := &pickerHTTPRecorder{}
	prevTransport := metadataTransport
	metadataTransport = recorder
	t.Cleanup(func() { metadataTransport = prevTransport })
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) { return -1, nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "1", nil })
	var confirm *ModelSelectionPrompt
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(_ PoolEnv, _ io.Writer, p ModelSelectionPrompt) (string, bool) {
		confirm = &p
		return "", false
	})
	r := pickerCaseRun(t, ctx, h, out)
	r.Config, r.CurrentModel = map[string]any{}, "gpt-5.5"
	if err := modelFlowOpenAICodex(r); err != nil {
		t.Fatal(err)
	}
	if hosts := recorder.authorizedHosts(); len(hosts) != 1 || !hosts["codex-gw.example"] {
		t.Fatalf("authorized hosts %v", hosts)
	}
	if confirm == nil || confirm.ConfirmAPIKey != opaque || confirm.ConfirmBaseURL != gw {
		t.Fatalf("confirm %+v", confirm)
	}
}

func TestModelCommandPromptsToReuseOrReauthenticateCodexSession(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) { return -1, nil })
	choices := []string{"2"}
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) {
		if len(choices) == 0 {
			return "", io.EOF
		}
		c := choices[0]
		choices = choices[1:]
		return c, nil
	})
	swapFlowSeam(t, &flowCodexStatusFn, func(PoolEnv) map[string]any { return map[string]any{"logged_in": true, "source": "hermes-auth-store"} })
	swapFlowSeam(t, &flowResolveCodexFn, func(PoolEnv, CodexResolveOptions) (map[string]any, error) {
		return map[string]any{"api_key": "fresh-codex-token"}, nil
	})
	loginCalls, forceNewLogin := 0, false
	swapFlowSeam(t, &flowLoginCodexFn, func(_ PoolEnv, _ io.Writer, _ CodexLoginArgs, force bool) error {
		loginCalls++
		forceNewLogin = force
		return nil
	})
	swapFlowSeam(t, &flowCodexModelIDsFn, func(context.Context, string, string) ([]string, bool) { return []string{"gpt-5.4", "gpt-5.5"}, false })
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "", false })
	r := pickerCaseRun(t, ctx, h, out)
	r.Config, r.CurrentModel = map[string]any{}, "gpt-5.4"
	if err := modelFlowOpenAICodex(r); err != nil {
		t.Fatal(err)
	}
	if loginCalls != 1 || !forceNewLogin {
		t.Fatalf("login calls %d force %v", loginCalls, forceNewLogin)
	}
}

func TestProviderModelIdsPreferredKimiSetupFlowUsesSameCodingPlanCatalog(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	swapFlowSeam(t, &promptAPIKeyFn, func(PoolEnv, io.Writer, Profile, string, string, string) (string, bool, error) {
		return "sk-kimi-test", false, nil
	})
	var captured []string
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(_ PoolEnv, _ io.Writer, p ModelSelectionPrompt) (string, bool) {
		captured = p.ModelIDs
		return "", false
	})
	r := pickerCaseRun(t, ctx, h, out)
	r.Config, r.CurrentModel = map[string]any{}, ""
	if err := modelFlowKimi(r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(captured, providerModels["kimi-coding"]) {
		t.Fatalf("models %v want %v", captured, providerModels["kimi-coding"])
	}
}

// Serves Ollama native /api/tags and rejects OpenAI /v1/models, recording every path like the hermes handler
func startFakeOllamaTagsServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		paths = append(paths, req.URL.RequestURI())
		mu.Unlock()
		switch strings.TrimRight(req.URL.Path, "/") {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"name":"qwen3:1.7b","model":"qwen3:1.7b"},{"name":"llama3.2:1b","model":"llama3.2:1b"}]}`)
		case "/v1/models":
			w.WriteHeader(503)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, paths...)
	}
}

// Radio list double recording the plain row labels and cancelling like the hermes side effect
func captureMenuAndCancel(t *testing.T) *[]string {
	t.Helper()
	items := &[]string{}
	swapFlowSeam(t, &flowRadiolistFn, func(_ string, rows [][]PickerSegment, _ PickerRadiolistOptions) (int, error) {
		for _, row := range rows {
			*items = append(*items, row[0].Text)
		}
		return -1, nil
	})
	return items
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if strings.TrimRight(p, "/") == want {
			return true
		}
	}
	return false
}

func TestLocalOllamaModelDiscoveryNamedCustomModelFlowDiscoversOllamaApiTags(t *testing.T) {
	baseURL, paths := startFakeOllamaTagsServer(t)
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "providers:\n  ollama:\n    base_url: "+baseURL+"\n")
	menu := captureMenuAndCancel(t)
	swapFlowSeam(t, &flowSaveModelChoiceFn, func(PoolEnv, string) error { return nil })
	swapFlowSeam(t, &saveCustomProviderFn, func(*ModelFlowRun, string, string, string, int, string, string, string) error { return nil })
	scriptBuiltinsInput(t, "manual-fallback")
	r := pickerCaseRun(t, ctx, h, out)
	if err := modelFlowNamedCustom(r, map[string]any{"name": "Local Ollama", "base_url": baseURL}); err != nil {
		t.Fatal(err)
	}
	seen := paths()
	if !reflect.DeepEqual(*menu, []string{"qwen3:1.7b", "llama3.2:1b", "Cancel"}) || !containsPath(seen, "/api/tags") || containsPath(seen, "/v1/models") {
		t.Fatalf("menu %v paths %v", *menu, seen)
	}
}

func TestLocalOllamaModelDiscoveryNamedCustomModelFlowPreservesExplicitOllamaModels(t *testing.T) {
	baseURL, paths := startFakeOllamaTagsServer(t)
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "providers:\n  ollama:\n    base_url: "+baseURL+"\n")
	menu := captureMenuAndCancel(t)
	r := pickerCaseRun(t, ctx, h, out)
	if err := modelFlowNamedCustom(r, map[string]any{"name": "Local Ollama", "base_url": baseURL, "models": []any{"curated-only"}}); err != nil {
		t.Fatal(err)
	}
	seen := paths()
	if !reflect.DeepEqual(*menu, []string{"curated-only", "Cancel"}) || containsPath(seen, "/api/tags") || containsPath(seen, "/v1/models") {
		t.Fatalf("menu %v paths %v", *menu, seen)
	}
}

// Scripts the Portal for the setup flow catalog and the GUI picker like the hermes _stub_portal helper
func scriptNousPortalPickers(t *testing.T, freeTier bool, pricing PricingTable, allowed map[string]bool) {
	t.Helper()
	swapFlowSeam(t, &pickerNousPricingFn, func(context.Context) PricingTable { return pricing })
	swapFlowSeam(t, &checkNousFreeTierFn, func(context.Context, bool, bool) bool { return freeTier })
	swapFlowSeam(t, &fetchNousRecommendedModelsFn, func(context.Context, string, bool) map[string]any { return nil })
	swapFlowSeam(t, &nousPolicyAllowedIDsFn, func(context.Context, bool) (map[string]bool, bool) { return allowed, allowed != nil })
}

func TestCliSetupFlowAndGuiPickerAgree(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	pricing := PricingTable{"nous/a": cloneMap(listPrice), "sale/x": salePrice(80), "sale/y": salePrice(30)}
	scriptNousPortalPickers(t, false, pricing, nil)
	cli := nousModelCatalog(pickerCaseRun(t, ctx, h, out), false, "", []string{"nous/a"}, pricing)
	if cli == nil {
		t.Fatal("no catalog")
	}
	if gui := nousPickerModelIDs(ctx, map[string][]string{"nous": {"nous/a"}}, false); !reflect.DeepEqual(cli.modelIDs, gui) {
		t.Fatalf("cli %v gui %v", cli.modelIDs, gui)
	}
}

func TestAllThreePaidPickersAgree(t *testing.T) {
	resetNousSeams(t)
	pricing := PricingTable{"curated/only": cloneMap(listPrice), "sale/x": salePrice(80), "sale/y": salePrice(30)}
	login := loginPickerIDs(t, false, pricing, nil)
	h, ctx, out := pickerCaseHome(t)
	scriptNousPortalPickers(t, false, pricing, nil)
	cli := nousModelCatalog(pickerCaseRun(t, ctx, h, out), false, "", []string{"curated/only"}, pricing)
	gui := nousPickerModelIDs(ctx, map[string][]string{"nous": {"curated/only"}}, false)
	want := []string{"curated/only", "sale/x", "sale/y"}
	if cli == nil || !reflect.DeepEqual(login, want) || !reflect.DeepEqual(cli.modelIDs, want) || !reflect.DeepEqual(gui, want) {
		t.Fatalf("login %v cli %+v gui %v", login, cli, gui)
	}
}

func TestStepfunPickerMergesLiveWithCuratedWizardOfferMergesLiveAndCurated(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("STEPFUN_BASE_URL", "")
	t.Setenv("STEPFUN_API_KEY", "")
	swapFlowSeam(t, &ensureFlowAPIKeyFn, func(PoolEnv, io.Writer, string, Profile, []string) (string, string, bool, error) {
		return "", "sk-stepfun-test", false, nil
	})
	swapFlowSeam(t, &setupPromptProviderChoiceFn, func(io.Writer, []string, int, string) (int, bool) { return 0, true })
	var captured []string
	swapFlowSeam(t, &pickModelOrPromptFn, func(_ PoolEnv, _ io.Writer, models []string, _ string, _ ModelSelectionPrompt) (string, bool) {
		captured = append([]string{}, models...)
		return "", false
	})
	swapFlowSeam(t, &finishModelFn, func(PoolEnv, io.Writer, string, string, string, string, persistModelOptions) (map[string]any, error) {
		return nil, nil
	})
	r := pickerCaseRun(t, ctx, h, out)
	r.Config = map[string]any{}
	if err := modelFlowStepfun(r); err != nil {
		t.Fatal(err)
	}
	lower := map[string]bool{}
	for _, m := range captured {
		lower[strings.ToLower(m)] = true
	}
	if !lower["step-3.7-flash"] {
		t.Fatalf("StepFun setup wizard offered the unmerged Step Plan list: %v", captured)
	}
	for _, id := range []string{"step-3.5-flash", "step-3.5-flash-2603"} {
		if !lower[id] {
			t.Fatalf("live id %s missing from %v", id, captured)
		}
	}
	for _, curated := range providerModels["stepfun"] {
		found := false
		for _, m := range captured {
			found = found || m == curated
		}
		if !found {
			t.Fatalf("curated %s missing from %v", curated, captured)
		}
	}
	if len(lower) != len(captured) {
		t.Fatalf("duplicate rows %v", captured)
	}
}

func TestSetupProbeCredentialsNeverBecomeSavedCredentials(t *testing.T) {
	for _, route := range []string{"providers", "custom_providers"} {
		for _, discover := range []any{true, false, "false"} {
			for _, static := range []map[string]any{{}, {"api_key": "stale"}, {"key_env": "TEST_GATEWAY_KEY"}} {
				t.Run(fmt.Sprintf("%s-%v-%v", route, discover, static), func(t *testing.T) {
					h, ctx, out := pickerCaseHome(t)
					t.Setenv("TEST_GATEWAY_KEY", "stale")
					entry := map[string]any{"name": "Gateway", "base_url": "https://gateway.invalid/v1", "key_cmd": "tenant-a",
						"model": "fallback", "default_model": "fallback", "discover_models": discover}
					for k, v := range static {
						entry[k] = v
					}
					cfg := map[string]any{"model": map[string]any{"default": "fallback", "provider": "custom"}}
					if route == "providers" {
						cfg[route] = map[string]any{"gateway": cloneMap(entry)}
					} else {
						cfg[route] = []any{cloneMap(entry)}
					}
					if err := config.Save(h, config.Tree(cfg)); err != nil {
						t.Fatal(err)
					}
					r := pickerCaseRun(t, ctx, h, out)
					infos, err := namedCustomProviderMap(r, r.Config)
					if err != nil || len(infos.keys) == 0 {
						t.Fatalf("map %v %v", infos, err)
					}
					info := infos.byKey[infos.keys[0]]
					var mints, probes, choices []string
					swapFlowSeam(t, &commandTokenMintFn, func(_ context.Context, s *CommandTokenSource) (string, time.Duration, error) {
						mints = append(mints, s.Command)
						return "transient-bearer", time.Hour, nil
					})
					swapFlowSeam(t, &flowFetchAPIModelsFn, func(_ context.Context, key, _ string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
						probes = append(probes, key)
						if key == "transient-bearer" {
							return []string{"fallback", "discovered"}, true
						}
						return nil, false
					})
					swapFlowSeam(t, &shouldUseOllamaNativeCatalogFn, func(context.Context, string, string, map[string]string) bool { return false })
					swapFlowSeam(t, &pickNamedCustomModelFn, func(_ *ModelFlowRun, _ string, available []string, _ string) (string, bool) {
						choices = append(choices, available...)
						return available[len(available)-1], true
					})
					swapFlowSeam(t, &flowAskFn, func(io.Writer, string, flowAskOptions) (string, bool) { return "fallback", true })
					if err := modelFlowNamedCustom(r, info); err != nil {
						t.Fatal(err)
					}
					if discover == true {
						if !reflect.DeepEqual(mints, []string{"tenant-a"}) || !reflect.DeepEqual(probes, []string{"transient-bearer"}) ||
							!reflect.DeepEqual(choices, []string{"fallback", "discovered"}) {
							t.Fatalf("mints %v probes %v choices %v", mints, probes, choices)
						}
					} else if len(mints) != 0 || len(probes) != 0 || !reflect.DeepEqual(choices, []string{"fallback"}) {
						t.Fatalf("mints %v probes %v choices %v", mints, probes, choices)
					}
					saved, err := config.Load(h, config.NewSecrets(h).Lookup())
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(fmt.Sprint(map[string]any(saved)), "transient-bearer") {
						t.Fatalf("bearer persisted %v", saved)
					}
					var persisted map[string]any
					if route == "providers" {
						persisted = saved[route].(map[string]any)["gateway"].(map[string]any)
					} else {
						persisted = saved[route].([]any)[0].(map[string]any)
					}
					if persisted["key_cmd"] != entry["key_cmd"] || pyOrStr(dictGet(persisted, "api_key", "")) != pyOrStr(dictGet(static, "api_key", "")) {
						t.Fatalf("persisted %v", persisted)
					}
				})
			}
		}
	}
}

func TestPickerCommandAuthIsLazyAndCredentialScoped(t *testing.T) {
	for _, route := range []string{"providers", "custom_providers"} {
		for _, static := range []map[string]any{{}, {"api_key": "stale"}, {"key_env": "TEST_GATEWAY_KEY"}} {
			t.Run(fmt.Sprintf("%s-%v", route, static), func(t *testing.T) {
				h, ctx, _ := pickerCaseHome(t)
				t.Setenv("TEST_GATEWAY_KEY", "stale")
				var mints, probes []string
				swapFlowSeam(t, &commandTokenMintFn, func(_ context.Context, s *CommandTokenSource) (string, time.Duration, error) {
					mints = append(mints, s.Command)
					if s.Command == "broken" {
						return "", 0, &CommandTokenError{"private helper output"}
					}
					return "token-" + s.Command, time.Hour, nil
				})
				swapFlowSeam(t, &fetchAPIModelsFn, func(_ context.Context, key, _ string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
					probes = append(probes, key)
					if strings.HasPrefix(key, "token-") {
						return []string{"fallback", "catalog-" + key}, true
					}
					return nil, false
				})
				swapFlowSeam(t, &shouldUseOllamaNativeCatalogFn, func(context.Context, string, string, map[string]string) bool { return false })
				entry := map[string]any{"name": "Gateway", "base_url": "https://gateway.invalid/v1", "key_cmd": "tenant-a", "model": "fallback", "default_model": "fallback"}
				for k, v := range static {
					entry[k] = v
				}
				with := func(extra map[string]any) map[string]any {
					out := cloneMap(entry)
					for k, v := range extra {
						out[k] = v
					}
					return out
				}
				rows := func(probe bool, entries ...map[string]any) []map[string]any {
					b := &pickerBuild{ctx: ctx, probeCustomProviders: probe, excluded: map[string]bool{}, curated: map[string][]string{},
						seenSlugs: map[string]bool{}, builtinEndpoints: map[string]bool{}, section3Pairs: map[[2]string]bool{}}
					if route == "providers" {
						user := map[string]any{}
						for i, e := range entries {
							user[fmt.Sprintf("gateway-%d", i)] = e
						}
						if err := lapUserProviderRows(b, user); err != nil {
							t.Fatal(err)
						}
					} else {
						list := make([]any, len(entries))
						for i, e := range entries {
							list[i] = e
						}
						if err := lapCustomProviderRows(b, list); err != nil {
							t.Fatal(err)
						}
					}
					return b.results
				}
				models := func(row map[string]any) []string { return inventoryModels(row) }
				for _, disabled := range []any{false, "false"} {
					if got := models(rows(true, with(map[string]any{"discover_models": disabled}))[0]); !reflect.DeepEqual(got, []string{"fallback"}) {
						t.Fatalf("disabled %v models %v", disabled, got)
					}
				}
				if got := models(rows(false, entry)[0]); !reflect.DeepEqual(got, []string{"fallback"}) || len(mints) != 0 || len(probes) != 0 {
					t.Fatalf("no probe models %v mints %v probes %v", got, mints, probes)
				}
				first := models(rows(true, entry)[0])
				if !reflect.DeepEqual(first, []string{"fallback", "catalog-token-tenant-a"}) || !reflect.DeepEqual(mints, []string{"tenant-a"}) || !reflect.DeepEqual(probes, []string{"token-tenant-a"}) {
					t.Fatalf("first %v mints %v probes %v", first, mints, probes)
				}
				for _, probe := range []bool{false, true} {
					if got := models(rows(probe, entry)[0]); !reflect.DeepEqual(got, first) {
						t.Fatalf("cached probe %v models %v", probe, got)
					}
				}
				if !reflect.DeepEqual(mints, []string{"tenant-a"}) || !reflect.DeepEqual(probes, []string{"token-tenant-a"}) {
					t.Fatalf("mints %v probes %v", mints, probes)
				}
				other := with(map[string]any{"key_cmd": "tenant-b", "discover_models": false})
				if err := config.Save(h, config.Tree{"custom_providers": []any{cloneMap(entry), other}}); err != nil {
					t.Fatal(err)
				}
				if route == "custom_providers" {
					rows(true, entry)
					saved, err := config.Load(h, config.NewSecrets(h).Lookup())
					if err != nil {
						t.Fatal(err)
					}
					if m := saved["custom_providers"].([]any)[1].(map[string]any)["models"]; m != nil {
						t.Fatalf("other entry gained models %v", m)
					}
				}
				source := BuildCommandTokenSource("tenant-a", "", h, nil)
				result, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: source, BaseURL: entry["base_url"].(string), ForceRefresh: true,
					FetchModels: func() (CatalogRows, bool) { return CatalogRows{IDs: []string{}, Native: true}, true }})
				if !result.Native || len(result.IDs) != 0 {
					t.Fatalf("native empty %+v", result)
				}
				cachedEmpty, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: source, BaseURL: entry["base_url"].(string), CacheOnly: true})
				if !cachedEmpty.Native || len(cachedEmpty.IDs) != 0 {
					t.Fatalf("cached native empty %+v", cachedEmpty)
				}
				for _, probe := range []bool{false, true} {
					row := rows(probe, entry)[0]
					if got := models(row); len(got) != 0 || row["native_catalog_empty"] != true {
						t.Fatalf("native row probe %v %v", probe, row)
					}
				}
				ClearAllProviderModelsCache(ctx)
				distinct := rows(true, entry, with(map[string]any{"key_cmd": "tenant-b"}))
				if !reflect.DeepEqual(models(distinct[0]), first) || !reflect.DeepEqual(models(distinct[1]), []string{"fallback", "catalog-token-tenant-b"}) {
					t.Fatalf("distinct %v", distinct)
				}
				if got := models(rows(true, with(map[string]any{"key_cmd": "broken"}))[0]); !reflect.DeepEqual(got, []string{"fallback"}) {
					t.Fatalf("broken models %v", got)
				}
				for _, p := range probes {
					if p == "stale" {
						t.Fatalf("stale key probed %v", probes)
					}
				}
			})
		}
	}
}
