package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestXAIRetirementApplyMigrationPreservesLongDoubleQuotedScalar(t *testing.T) {
	h := newTestHome(t)
	value := strings.Repeat("A", 74) + `D:\CentBrowserPortable ` + strings.Repeat("B", 40)
	cfg := filepath.Join(h, "config.yaml")
	text := "model:\n  provider: xai\n  model: grok-3\napprovals:\n  smart_policy: \"" + strings.ReplaceAll(value, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ApplyXAIRetirementMigration(t.Context(), cfg, []RetirementIssue{{ConfigPath: "model.model", CurrentModel: "grok-3", Replacement: "grok-4"}}, false)
	if err != nil || !res.ConfigChanged || len(res.IssuesResolved) != 1 {
		t.Fatalf("%v %v", res, err)
	}
	raw, _ := os.ReadFile(cfg)
	var loaded map[string]any
	if err := yaml.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded["model"].(map[string]any)["model"] != "grok-4" || loaded["approvals"].(map[string]any)["smart_policy"] != value {
		t.Fatalf("%s", raw)
	}
}

func TestXAIRetirementNormalizeAndLooksLikeXAI(t *testing.T) {
	if xaiRetirementNormalize("x-ai/grok-4") != "grok-4" || xaiRetirementNormalize(" XAI/Grok-3 ") != "grok-3" {
		t.Fatal("normalize")
	}
	for _, m := range []any{"gpt-4", "claude-sonnet-4-6", "openrouter/openai/gpt-4", nil, 3} {
		if xaiRetirementLooksLikeXAI(m) {
			t.Fatal(m)
		}
	}
}

func TestXAIRetirementFindRefs(t *testing.T) {
	for _, cfg := range []any{map[string]any{}, nil, "nope", map[string]any{
		"principal": map[string]any{"provider": "openai", "model": "gpt-4o"}, "auxiliary": map[string]any{"vision": map[string]any{"model": "claude-sonnet-4-6"}},
		"delegation": map[string]any{"model": "openai/o3"}}} {
		if got := FindRetiredXAIRefs(cfg); len(got) != 0 {
			t.Fatalf("%v", got)
		}
	}
	issues := FindRetiredXAIRefs(map[string]any{"principal": map[string]any{"model": "grok-code-fast-1"}})
	if len(issues) != 1 || issues[0].ConfigPath != "principal.model" || issues[0].CurrentModel != "grok-code-fast-1" || issues[0].Replacement != "grok-4.3" || issues[0].ReasoningEffort != "" {
		t.Fatalf("%v", issues)
	}
	issues = FindRetiredXAIRefs(map[string]any{
		"auxiliary": map[string]any{"vision": map[string]any{"model": "x-ai/grok-4-fast-non-reasoning"}, "bad": "x"},
		"tts":       map[string]any{"xai": map[string]any{"model": "grok-3"}},
		"plugins":   map[string]any{"image_gen": map[string]any{"xai": map[string]any{"model": "grok-imagine-image-pro"}}}})
	if len(issues) != 3 || issues[0].ReasoningEffort != "none" || issues[2].Replacement != "grok-imagine-image-quality" {
		t.Fatalf("%v", issues)
	}
	if got := FormatRetirementIssue(issues[0]); got != `auxiliary.vision.model: 'x-ai/grok-4-fast-non-reasoning' `+"\u2192"+` use 'grok-4.3' (set reasoning_effort: "none")` {
		t.Fatal(got)
	}
	for name, entry := range xaiRetiredModels {
		if entry.Replacement == "" {
			t.Fatal(name)
		}
	}
}

func TestZaiDetectedBaseURLCachesSuccessAndBacksOffFailures(t *testing.T) {
	h := newTestHome(t)
	var probes atomic.Int32
	prev := detectZaiEndpointFn
	t.Cleanup(func() { detectZaiEndpointFn = prev })
	detectZaiEndpointFn = func(_ context.Context, apiKey string, _ time.Duration) map[string]string {
		probes.Add(1)
		if apiKey == "bad" {
			return nil
		}
		return map[string]string{"id": "coding-global", "base_url": "https://api.z.ai/api/coding/paas/v4", "model": "glm-5.3", "label": "Global (Coding Plan)"}
	}
	for i := 0; i < 2; i++ {
		if got := resolveZaiBaseURLDetected(t.Context(), h, "good", "https://def"); got != "https://api.z.ai/api/coding/paas/v4" {
			t.Fatal(got)
		}
	}
	for i := 0; i < 2; i++ {
		if got := resolveZaiBaseURLDetected(t.Context(), h, "bad", "https://def"); got != "https://def" {
			t.Fatal(got)
		}
	}
	state := readAuthStore(t, h)["providers"].(map[string]any)["zai"].(map[string]any)["detected_endpoint"].(map[string]any)
	if probes.Load() != 2 || state["endpoint_id"] != "coding-global" || len(anyString(state["key_hash"])) != 16 || readAuthStore(t, h)["active_provider"] != nil {
		t.Fatalf("%d %v", probes.Load(), state)
	}
}

func TestZaiDetectPrefersHigherPriorityEndpoint(t *testing.T) {
	scriptClient(t, &zaiHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &body)
		if req.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header %q", req.Header.Get("Authorization"))
		}
		if strings.Contains(req.URL.Host, "bigmodel") || (strings.Contains(req.URL.Path, "/coding/") && body["model"] == "glm-5.2") {
			time.Sleep(20 * time.Millisecond)
			return textResponse(200, "{}"), nil
		}
		return textResponse(401, "{}"), nil
	})
	got := detectZaiEndpoint(t.Context(), "k", time.Second)
	if got["id"] != "cn" || got["model"] != "glm-5" {
		t.Fatalf("%v", got)
	}
}

func TestKimiBaseURLFromKeyPrefix(t *testing.T) {
	if resolveKimiBaseURL("sk-kimi-x", "https://d", "") != KimiCodeBaseURL || resolveKimiBaseURL("sk-kimi-x", "https://d", "https://e") != "https://e" ||
		resolveKimiBaseURL("legacy", "https://d", "") != "https://d" {
		t.Fatal("kimi base url")
	}
}

func TestPluginOAuthPoolEntryExpiry(t *testing.T) {
	if !poolEntryExpired(PooledCredential{ExpiresAtMS: time.Now().UnixMilli() - 1}) || poolEntryExpired(PooledCredential{ExpiresAtMS: time.Now().UnixMilli() + 60000}) ||
		!poolEntryExpired(PooledCredential{ExpiresAt: isoIn(-10)}) || poolEntryExpired(PooledCredential{ExpiresAt: isoIn(600)}) || poolEntryExpired(PooledCredential{}) {
		t.Fatal("expiry")
	}
}

func TestPluginOAuthDispatchOffersTheAuthHandler(t *testing.T) {
	registerHookedProfile(t, "plugtest", ProviderProfileHooks{AuthHandler: func(action string, args []string, provider string) (bool, error) {
		if action == "add" {
			return true, nil
		}
		return false, os.ErrPermission
	}})
	if owned, err := dispatchPluginAuth("add", nil, "plugtest"); !owned || err != nil {
		t.Fatal(owned, err)
	}
	if _, err := dispatchPluginAuth("logout", nil, "plugtest"); err == nil || err.Error() != "plugtest auth handler failed for `logout`: errorString: permission denied" {
		t.Fatalf("%v", err)
	}
	if owned, err := dispatchPluginAuth("add", nil, "no-such-plugin"); owned || err != nil {
		t.Fatal(owned, err)
	}
}

func TestOpenRouterAuthExchangeCode(t *testing.T) {
	status, body := 200, `{"key":" sk-or-v1-abc "}`
	scriptClient(t, &openrouterHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		var sent map[string]string
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &sent)
		if sent["code"] != "c" || sent["code_verifier"] != "v" || sent["code_challenge_method"] != "S256" {
			t.Errorf("%v", sent)
		}
		return textResponse(status, body), nil
	})
	if key, err := openrouterExchangeCode(t.Context(), "c", "v", time.Second); err != nil || key != "sk-or-v1-abc" {
		t.Fatal(key, err)
	}
	for _, tc := range []struct {
		status        int
		body, code    string
		relogin       bool
		messagePrefix string
	}{
		{403, "", "openrouter_token_exchange_denied", true, "OpenRouter rejected the authorization code"},
		{500, strings.Repeat("e", 3000), "openrouter_token_exchange_failed", false, "OpenRouter code exchange failed (HTTP 500). Response: eee"},
		{200, "nope", "openrouter_token_exchange_invalid", false, "OpenRouter code exchange returned a non-JSON body."},
		{200, `{"key":""}`, "openrouter_token_exchange_invalid", false, "OpenRouter code exchange response did not include a 'key'."},
	} {
		status, body = tc.status, tc.body
		_, err := openrouterExchangeCode(t.Context(), "c", "v", time.Second)
		ae, ok := err.(*AuthError)
		if !ok || ae.Code != tc.code || ae.ReloginRequired != tc.relogin || !strings.HasPrefix(ae.Message, tc.messagePrefix) || len(ae.Message) > 2200 {
			t.Fatalf("%d %v", tc.status, err)
		}
	}
}

func TestOpenRouterAuthLoopbackCodeAndDenial(t *testing.T) {
	out := &lockedWriter{}
	prev := openBrowserFn
	t.Cleanup(func() { openBrowserFn = prev })
	callback := "code=the-code"
	openBrowserFn = func(string) error { return nil }
	go func() {
		for i := 0; i < 200; i++ {
			time.Sleep(10 * time.Millisecond)
			s := out.String()
			if i := strings.Index(s, "callback_url="); i >= 0 {
				raw := s[i+len("callback_url="):]
				raw = raw[:strings.IndexAny(raw, "&\n")]
				target, _ := url.QueryUnescape(raw)
				resp, err := http.Get(target + "?" + callback)
				if err == nil {
					resp.Body.Close()
				}
				return
			}
		}
	}()
	code, err := openrouterLoopbackCode(out, [][2]string{{"code_challenge", "ch"}, {"code_challenge_method", "S256"}}, false, 3*time.Second)
	if err != nil || code != "the-code" {
		t.Fatal(code, err)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
