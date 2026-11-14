package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const moonshotDefaultURL = "https://api.moonshot.ai/v1"

// Temp home with every provider key cleared and an empty auth store, the autouse fixture of the file
func apiKeyProvidersEnv(t *testing.T) PoolEnv {
	t.Helper()
	clearProviderEnv(t)
	for _, name := range []string{"NOUS_API_KEY", "LM_BASE_URL", "KIMI_BASE_URL", "STEPFUN_BASE_URL", "KILOCODE_BASE_URL", "GMI_BASE_URL",
		"OPENAI_BASE_URL", "ICEBATH_COPILOT_ACP_COMMAND", "COPILOT_CLI_PATH", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_ACP_BASE_URL", "GLM_BASE_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return testEnv(t, newTestHome(t))
}

func noZaiProbe(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	prev := detectZaiEndpointFn
	detectZaiEndpointFn = func(context.Context, string, time.Duration) map[string]string { calls.Add(1); return nil }
	t.Cleanup(func() { detectZaiEndpointFn = prev })
	return &calls
}

func TestResolveProviderAliases(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	for in, want := range map[string]string{"zhipu": "zai", "kimi": "kimi-coding", "moonshot": "kimi-coding", "step": "stepfun",
		"minimax_cn": "minimax-cn", "vercel": "ai-gateway", "kilo": "kilocode", "GLM": "zai", "Z-AI": "zai", "Kimi": "kimi-coding",
		"chatgpt": "openai-codex", "chatgpt-codex": "openai-codex", "hf": "huggingface"} {
		if got, err := resolveProvider(env, in); err != nil || got != want {
			t.Errorf("resolve_provider(%q) = %q %v, want %q", in, got, err, want)
		}
	}
}

func TestAliasChatgptEveryAliasTable(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	for _, alias := range []string{"chatgpt", "chatgpt-codex"} {
		if got := NormalizeProviderDef(ctx, alias); got != "openai-codex" {
			t.Errorf("providers %q = %q", alias, got)
		}
		if p, m := ParseModelInput(ctx, alias+":gpt-5.5", "openrouter", nil); p != "openai-codex" || m != "gpt-5.5" {
			t.Errorf("parse_model_input %q = %q %q", alias, p, m)
		}
	}
}

func TestUnknownProviderRaises(t *testing.T) {
	var ae *AuthError
	if _, err := resolveProvider(apiKeyProvidersEnv(t), "nonexistent-provider-xyz"); !errors.As(err, &ae) {
		t.Fatalf("want AuthError, got %v", err)
	}
}

func TestAutoDetectsAPIKeyProviders(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"GLM_API_KEY": "test-glm-key"}, "zai"},
		{map[string]string{"KIMI_API_KEY": "test-kimi-key"}, "kimi-coding"},
		{map[string]string{"MINIMAX_CN_API_KEY": "test-mm-cn-key"}, "minimax-cn"},
		{map[string]string{"OPENROUTER_API_KEY": "or-key", "GLM_API_KEY": "glm-key"}, "openrouter"},
	} {
		t.Run(c.want, func(t *testing.T) {
			env := apiKeyProvidersEnv(t)
			anonNoAWS(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got, err := resolveProvider(env, "auto"); err != nil || got != c.want {
				t.Fatalf("auto = %q %v", got, err)
			}
		})
	}
}

func TestAutoDoesNotSelectCopilotFromGithubToken(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	anonNoAWS(t)
	t.Setenv("GITHUB_TOKEN", "gh-test-token")
	_, err := resolveProvider(env, "auto")
	var ae *AuthError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "not connected to any AI provider") {
		t.Fatalf("want the not connected AuthError, got %v", err)
	}
}

func TestAPIKeyProviderStatusConfiguredAndNot(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	noZaiProbe(t)
	status := GetAPIKeyProviderStatus(env, "zai")
	if status["configured"] != false || status["logged_in"] != false {
		t.Fatalf("unconfigured %v", status)
	}
	t.Setenv("GLM_API_KEY", "test-key-123")
	status = GetAPIKeyProviderStatus(env, "zai")
	base, _ := status["base_url"].(string)
	if status["configured"] != true || status["logged_in"] != true || status["key_source"] != "GLM_API_KEY" ||
		!(strings.Contains(strings.ToLower(base), "z.ai") || strings.Contains(base, "api.z.ai")) {
		t.Fatalf("configured %v", status)
	}
}

func TestResolveStepfunWithKey(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	t.Setenv("STEPFUN_API_KEY", "stepfun-secret-key")
	creds, err := authResolver(env).apiKeyProviderCredentials("stepfun")
	if err != nil || creds.APIKey != "stepfun-secret-key" || creds.BaseURL != StepfunStepPlanIntlBaseURL {
		t.Fatalf("%+v %v", creds, err)
	}
}

func TestRuntimeAPIKeyProviders(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	noZaiProbe(t)
	t.Setenv("GLM_API_KEY", "glm-key")
	rt := mustResolve(t, r, "zai", "", "", "")
	if rt.Provider != "zai" || rt.APIMode != ModeChatCompletions || rt.APIKey != "glm-key" || !strings.Contains(rt.BaseURL, "z.ai") {
		t.Fatalf("zai %+v", rt)
	}
	t.Setenv("MINIMAX_API_KEY", "mm-key")
	if rt := mustResolve(t, r, "minimax", "", "", ""); rt.Provider != "minimax" || rt.APIKey != "mm-key" {
		t.Fatalf("minimax %+v", rt)
	}
}

func TestRuntimeAutoDetectsAPIKeyProvider(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	anonNoAWS(t)
	t.Setenv("KIMI_API_KEY", "auto-kimi-key")
	if rt := mustResolve(t, r, "auto", "", "", ""); rt.Provider != "kimi-coding" || rt.APIKey != "auto-kimi-key" {
		t.Fatalf("auto %+v", rt)
	}
}

func TestRuntimeExternalProcessProviderUsesProcessRuntime(t *testing.T) {
	snapshotProviderRegistry(t)
	r := testResolver(t, nil, &fakeCreds{}, "")
	registerPluginForTest(t, Profile{Name: "acme-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://acme",
		ProcessCommand: "acme", ProcessArgs: []string{"--acp", "--stdio"}, ProcessArgsEnvVar: "ICEBATH_ACME_ACP_ARGS"})
	dir := t.TempDir()
	cli := filepath.Join(dir, "acme")
	if err := os.WriteFile(cli, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ICEBATH_ACME_ACP_ARGS", "--acp --stdio --debug")
	rt := mustResolve(t, r, "acme-acp", "", "", "")
	if rt.Provider != "acme-acp" || rt.APIMode != ModeChatCompletions || rt.APIKey != "acme-acp" || rt.BaseURL != "acp://acme" ||
		!reflect.DeepEqual(rt.Args, []string{"--acp", "--stdio", "--debug"}) {
		t.Fatalf("%+v", rt)
	}
	got, err1 := os.Stat(rt.Command)
	want, err2 := os.Stat(cli)
	if err1 != nil || err2 != nil || !os.SameFile(got, want) {
		t.Fatalf("command %q", rt.Command)
	}
}

func TestResolveKimiBaseURLPrefixAndOverride(t *testing.T) {
	if got := resolveKimiBaseURL("sk-kimi-abc123", moonshotDefaultURL, ""); got != KimiCodeBaseURL {
		t.Fatalf("sk-kimi %q", got)
	}
	if got := resolveKimiBaseURL("sk-abc123", moonshotDefaultURL, "https://custom.example.com/v1"); got != "https://custom.example.com/v1" {
		t.Fatalf("override %q", got)
	}
}

func TestKimiCodeStatusEnvOverrideWins(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	t.Setenv("KIMI_API_KEY", "sk-kimi-test-key")
	t.Setenv("KIMI_BASE_URL", "https://override.example/v1")
	if got := GetAPIKeyProviderStatus(env, "kimi-coding")["base_url"]; got != "https://override.example/v1" {
		t.Fatalf("%v", got)
	}
}

func TestKimiCodeCredentialAutoDetect(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	noZaiProbe(t)
	t.Setenv("KIMI_API_KEY", "sk-legacy-secret-key")
	if creds, err := authResolver(env).apiKeyProviderCredentials("kimi-coding"); err != nil || creds.APIKey != "sk-legacy-secret-key" || creds.BaseURL != moonshotDefaultURL {
		t.Fatalf("legacy %+v %v", creds, err)
	}
	t.Setenv("GLM_API_KEY", "sk-kim...isnt")
	if creds, err := authResolver(env).apiKeyProviderCredentials("zai"); err != nil || creds.BaseURL != "https://api.z.ai/api/paas/v4" {
		t.Fatalf("zai %+v %v", creds, err)
	}
}

func TestZaiEndpointAutoDetectSkipsAndBacksOff(t *testing.T) {
	env := apiKeyProvidersEnv(t)
	calls := noZaiProbe(t)
	t.Setenv("GLM_API_KEY", "glm-key")
	t.Setenv("GLM_BASE_URL", "https://custom.example/v4")
	if creds, _ := authResolver(env).apiKeyProviderCredentials("zai"); creds.BaseURL != "https://custom.example/v4" || calls.Load() != 0 {
		t.Fatalf("env override %+v probes %d", creds, calls.Load())
	}
	t.Setenv("GLM_BASE_URL", "")
	t.Setenv("GLM_API_KEY", "")
	if creds, _ := authResolver(env).apiKeyProviderCredentials("zai"); creds.APIKey != "" || calls.Load() != 0 {
		t.Fatalf("no key %+v probes %d", creds, calls.Load())
	}
	zaiProbeMu.Lock()
	prevFailed := zaiProbeFailedUntil
	zaiProbeFailedUntil = map[string]float64{}
	zaiProbeMu.Unlock()
	t.Cleanup(func() { zaiProbeMu.Lock(); zaiProbeFailedUntil = prevFailed; zaiProbeMu.Unlock() })
	t.Setenv("GLM_API_KEY", "glm-key-that-429s")
	for i := 0; i < 3; i++ {
		if creds, _ := authResolver(env).apiKeyProviderCredentials("zai"); creds.BaseURL != "https://api.z.ai/api/paas/v4" {
			t.Fatalf("round %d %+v", i, creds)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("failed probe repeated %d times", calls.Load())
	}
}

// Scripted z.ai probe transport answering 200 for the listed endpoint and model pairs, delaying hosts by prefix
func scriptZaiProbe(t *testing.T, ok map[[2]string]bool, delay func(url string) time.Duration) {
	t.Helper()
	scriptClient(t, &zaiHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		body := jsonMap(string(raw))
		url := req.URL.String()
		if d := delay(url); d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		base := url[:strings.LastIndex(url, "/chat/completions")]
		if ok[[2]string{base, anyString(body["model"])}] {
			return textResponse(200, "{}"), nil
		}
		return textResponse(401, "{}"), nil
	})
}

func noDelay(string) time.Duration { return 0 }

func TestZaiCandidateModelFallbackWithinEndpoint(t *testing.T) {
	var codingGlobal zaiEndpoint
	for _, ep := range ZaiEndpoints {
		if ep.ID == "coding-global" {
			codingGlobal = ep
		}
	}
	last := codingGlobal.ProbeModels[len(codingGlobal.ProbeModels)-1]
	scriptZaiProbe(t, map[[2]string]bool{{codingGlobal.BaseURL, last}: true}, noDelay)
	got := detectZaiEndpoint(t.Context(), "test-key", time.Second)
	if got == nil || got["id"] != "coding-global" || got["model"] != last {
		t.Fatalf("%v", got)
	}
}

func TestZaiPriorityOrderWinsOverCompletionOrder(t *testing.T) {
	first, last := ZaiEndpoints[0], ZaiEndpoints[len(ZaiEndpoints)-1]
	scriptZaiProbe(t, map[[2]string]bool{{first.BaseURL, first.ProbeModels[0]}: true, {last.BaseURL, last.ProbeModels[0]}: true}, func(url string) time.Duration {
		if strings.HasPrefix(url, first.BaseURL) {
			return 150 * time.Millisecond
		}
		return 0
	})
	if got := detectZaiEndpoint(t.Context(), "test-key", time.Second); got == nil || got["id"] != first.ID {
		t.Fatalf("%v", got)
	}
}

func TestZaiAllFailReturnsNone(t *testing.T) {
	scriptZaiProbe(t, map[[2]string]bool{}, noDelay)
	if got := detectZaiEndpoint(t.Context(), "bad-key", time.Second); got != nil {
		t.Fatalf("%v", got)
	}
}

func TestZaiEarlyExitDoesNotWaitForSlowLosers(t *testing.T) {
	first := ZaiEndpoints[0]
	scriptZaiProbe(t, map[[2]string]bool{{first.BaseURL, first.ProbeModels[0]}: true}, func(url string) time.Duration {
		if !strings.HasPrefix(url, first.BaseURL) {
			return 2 * time.Second
		}
		return 0
	})
	start := time.Now()
	got := detectZaiEndpoint(t.Context(), "test-key", 5*time.Second)
	if elapsed := time.Since(start); got == nil || got["id"] != first.ID || elapsed >= 1500*time.Millisecond {
		t.Fatalf("%v after %v", got, elapsed)
	}
}

func TestMoonshotListExcludesCodingPlanOnlyModels(t *testing.T) {
	for _, m := range providerModels["moonshot"] {
		if m == "kimi-for-coding" || m == "kimi-k2-thinking-turbo" {
			t.Errorf("moonshot list leaks %q", m)
		}
	}
}

func TestHuggingFaceModelMetadataHasContextLengths(t *testing.T) {
	keys := map[string]bool{}
	for _, e := range defaultContextLengths {
		keys[strings.ToLower(e.key)] = true
	}
	for _, m := range providerModels["huggingface"] {
		if !keys[strings.ToLower(m)] {
			t.Errorf("HF model %q missing from the default context lengths", m)
		}
	}
}
