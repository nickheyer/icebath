package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Asker that picks the row matching each title and types the text mapped to a title
func scriptedAsker(t *testing.T, rows map[string]string, typed map[string]string) (SetupAsker, *[]HostedPrompt) {
	t.Helper()
	var asked []HostedPrompt
	return func(p HostedPrompt) HostedAnswer {
		asked = append(asked, p)
		switch p.Kind {
		case HostedChoice:
			for title, word := range rows {
				if !strings.Contains(p.Title, title) {
					continue
				}
				for i, row := range p.Rows {
					if row == word {
						return HostedAnswer{Index: i}
					}
				}
				for i, row := range p.Rows {
					if strings.Contains(row, word) {
						return HostedAnswer{Index: i}
					}
				}
			}
			return HostedAnswer{Index: p.Default}
		case HostedCommand:
			return HostedAnswer{Err: "no terminal in this test"}
		}
		for title, text := range typed {
			if strings.Contains(p.Title, title) {
				return HostedAnswer{Text: text}
			}
		}
		return HostedAnswer{Cancelled: true}
	}, &asked
}

// Home whose model flows find no external programs and no ambient provider keys
func hostedFlowHome(t *testing.T) (string, *ModelFlowRun) {
	t.Helper()
	h := newTestHome(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "VERTEX_CREDENTIALS_PATH",
		"GOOGLE_APPLICATION_CREDENTIALS", "COPILOT_CLI_PATH", "ICEBATH_COPILOT_ACP_COMMAND"} {
		t.Setenv(k, "")
	}
	swapFlowSeam(t, &oauthSleep, func(time.Duration) {})
	r, _ := newFlowRun(t, h)
	return h, r
}

func TestHostedQwenFlowSignsInByDeviceCodeAndSavesTheModel(t *testing.T) {
	h, r := hostedFlowHome(t)
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/device/code":
			if req.Form.Get("code_challenge_method") != "S256" || req.Form.Get("client_id") != QwenOAuthClientID {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"device_code": "dc", "user_code": "QW-1", "verification_uri_complete": "https://chat.qwen.ai/authorize?user_code=QW-1", "expires_in": 600}`))
		case "/token":
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token": "qa", "refresh_token": "qr", "token_type": "Bearer", "expires_in": 3600, "resource_url": "portal.qwen.ai"}`))
		}
	}))
	defer server.Close()
	swapFlowSeam(t, &qwenOAuthDeviceCodeURL, server.URL+"/device/code")
	swapFlowSeam(t, &qwenOAuthTokenURL, server.URL+"/token")
	swapFlowSeam(t, &openBrowserFn, func(string) error { return nil })
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"qwen3-coder-plus", "qwen3-coder-flash"}, true
	})
	ask, asked := scriptedAsker(t, map[string]string{"Qwen sign in": "Sign in", "model": "qwen3-coder-flash"}, nil)
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedQwenFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".qwen", "oauth_creds.json"))
	if err != nil {
		t.Fatalf("credentials %v\n%s", err, out.String())
	}
	var creds map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil || creds["access_token"] != "qa" || creds["refresh_token"] != "qr" || creds["expiry_date"] == nil {
		t.Fatalf("credentials %s", raw)
	}
	model := readConfig(t, h)["model"].(map[string]any)
	if model["provider"] != "qwen-oauth" || model["default"] != "qwen3-coder-flash" || polls != 2 {
		t.Fatalf("model %v polls %d asked %v", model, polls, *asked)
	}
	if !strings.Contains(out.String(), "QW-1") || strings.Contains(out.String(), "qwen auth qwen-oauth") {
		t.Fatalf("output %s", out.String())
	}
}

func TestHostedQwenDeviceLoginStopsWhenTheFlowIsCancelled(t *testing.T) {
	_, r := hostedFlowHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/device/code" {
			_, _ = w.Write([]byte(`{"device_code": "dc", "user_code": "QW-2", "verification_uri": "https://chat.qwen.ai/authorize", "expires_in": 600}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error": "authorization_pending"}`))
	}))
	defer server.Close()
	swapFlowSeam(t, &qwenOAuthDeviceCodeURL, server.URL+"/device/code")
	swapFlowSeam(t, &qwenOAuthTokenURL, server.URL+"/token")
	ctx, cancel := context.WithCancel(r.Ctx)
	r.Env.Ctx, r.Ctx = ctx, ctx
	slept := 0
	swapFlowSeam(t, &oauthSleep, func(time.Duration) {
		if slept++; slept == 3 {
			cancel()
		}
	})
	if err := qwenDeviceCodeLogin(r.Env, io.Discard, false); !isSignInCancelled(err) {
		t.Fatalf("a cancelled flow ended the poll with %v", err)
	}
}

func TestHostedAnthropicFlowSignsInInTheBrowserWithoutClaudeCode(t *testing.T) {
	h, r := hostedFlowHome(t)
	swapFlowSeam(t, &authCmdAnthropicOAuthLoginFn, func(*authCLI) map[string]any {
		return map[string]any{"access_token": "sk-ant-oat01-test", "refresh_token": "sk-ant-ort01-test", "expires_at_ms": float64(4102444800000)}
	})
	ask, asked := scriptedAsker(t, map[string]string{"authentication method": "browser", "model": "claude-sonnet-5"}, nil)
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedAnthropicFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	entries := LoadPool(r.Env, "anthropic").Entries()
	if len(entries) != 1 || entries[0].AccessToken != "sk-ant-oat01-test" || entries[0].Source != SourceManual+":icebath_pkce" {
		t.Fatalf("pool %+v\n%s", entries, out.String())
	}
	model := readConfig(t, h)["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["default"] != "claude-sonnet-5" {
		t.Fatalf("model %v asked %v", model, *asked)
	}
	if env, _ := os.ReadFile(home.EnvPath(h)); strings.Contains(string(env), "sk-ant-oat01-test") {
		t.Fatal("the pooled sign in must not land in .env")
	}
	ask, _ = scriptedAsker(t, map[string]string{"credentials": "Use existing", "model": "claude-opus-5"}, nil)
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedAnthropicFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	if model := readConfig(t, h)["model"].(map[string]any); model["default"] != "claude-opus-5" || len(LoadPool(r.Env, "anthropic").Entries()) != 1 {
		t.Fatalf("pooled credentials were not reused %v", model)
	}
}

func TestHostedVertexFlowSavesAServiceAccountFile(t *testing.T) {
	h, r := hostedFlowHome(t)
	sa := filepath.Join(t.TempDir(), "sa.json")
	vertexWriteSA(t, sa, gcpOAuth2TokenEndpoint, "p1", nil)
	ask, _ := scriptedAsker(t, map[string]string{"Vertex credentials": "service account", "model": "gemini"},
		map[string]string{"Service account JSON path": sa, "GCP project ID": "p1", "Vertex region": "us-central1"})
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedVertexFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(home.EnvPath(h)); !strings.Contains(string(env), "VERTEX_CREDENTIALS_PATH="+sa) {
		t.Fatalf("env %q\n%s", env, out.String())
	}
	cfg := readConfig(t, h)
	if cfg["model"].(map[string]any)["provider"] != "vertex" || cfg["vertex"].(map[string]any)["region"] != "us-central1" {
		t.Fatalf("config %v", cfg)
	}
}

func TestHostedEntraProbeSavesAServicePrincipalAndProbesAgain(t *testing.T) {
	h, r := hostedFlowHome(t)
	probes := 0
	probe := func(context.Context, *EntraIdentityConfig, string, time.Duration) map[string]any {
		probes++
		return map[string]any{"ok": probes > 1 && os.Getenv("AZURE_CLIENT_SECRET") == "s3cret"}
	}
	t.Setenv("AZURE_CLIENT_SECRET", "")
	ask, _ := scriptedAsker(t, map[string]string{"Entra": "service principal"}, map[string]string{"Tenant ID": "t1", "Client ID": "c1", "Client secret": "s3cret"})
	var info map[string]any
	if err := HostSetupFlow(ask, io.Discard, func() error {
		info = hostedEntraProbe(probe, ask)(r.Ctx, &EntraIdentityConfig{}, ScopeAIAzureDefault, time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(home.EnvPath(h))
	if info["ok"] != true || probes != 2 || !strings.Contains(string(env), "AZURE_TENANT_ID=t1") || !strings.Contains(string(env), "AZURE_CLIENT_ID=c1") {
		t.Fatalf("info %v probes %d env %q", info, probes, env)
	}
}

func TestHostedOpenRouterFlowMintsTheKeyInTheBrowser(t *testing.T) {
	h, r := hostedFlowHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body["code"] != "or-code" || body["code_challenge_method"] != "S256" {
			w.WriteHeader(403)
			return
		}
		_, _ = w.Write([]byte(`{"key": "sk-or-v1-minted"}`))
	}))
	defer server.Close()
	swapFlowSeam(t, &openrouterAuthKeysURL, server.URL)
	scriptBrowserEnv(t, map[string]string{"DISPLAY": ":0", "BROWSER": "firefox"})
	swapFlowSeam(t, &openBrowserFn, func(target string) error {
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		go func() { _, _ = http.Get(parsed.Query().Get("callback_url") + "?code=or-code") }()
		return nil
	})
	ask, _ := scriptedAsker(t, map[string]string{"OpenRouter": "Sign in", "model": "anthropic/"}, nil)
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedOpenRouterFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(home.EnvPath(h)); !strings.Contains(string(env), "OPENROUTER_API_KEY=sk-or-v1-minted") {
		t.Fatalf("env %q\n%s", env, out.String())
	}
	if model := readConfig(t, h)["model"].(map[string]any); model["provider"] != "openrouter" {
		t.Fatalf("model %v\n%s", model, out.String())
	}
}

func TestHostedCopilotACPFlowInstallsTheCLIThroughTheTerminal(t *testing.T) {
	_, r := hostedFlowHome(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var commands [][]string
	ask := func(p HostedPrompt) HostedAnswer {
		if p.Kind == HostedCommand {
			commands = append(commands, p.Argv)
			return HostedAnswer{Ran: true}
		}
		return HostedAnswer{Index: 0}
	}
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return hostedCopilotACPFlow(r, ask) }); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || strings.Join(commands[0][1:], " ") != "install -g @github/copilot" {
		t.Fatalf("commands %v\n%s", commands, out.String())
	}
	if !strings.Contains(out.String(), "COPILOT_CLI_PATH") {
		t.Fatalf("the wizard copy of a CLI still missing after the install is gone\n%s", out.String())
	}
}

func TestHostedExternalProcessLoginRunsOnTheTUITerminal(t *testing.T) {
	status := map[string]any{"available": true, "logged_in": false, "plan": "", "login_command": []string{"proc-cli", "auth", "login"},
		"detail": "Proc CLI is installed but not logged in. Run `proc-cli auth login`, then select this provider again."}
	profile := procProviderProfile(t, status, nil)
	registerTestProfile(t, profile)
	h := newTestHome(t)
	r, _ := newFlowRun(t, h)
	var commands [][]string
	ask := func(p HostedPrompt) HostedAnswer {
		if p.Kind == HostedCommand {
			commands = append(commands, p.Argv)
			status["logged_in"] = true
			return HostedAnswer{Ran: true}
		}
		return HostedAnswer{Index: p.Default}
	}
	var out strings.Builder
	if err := HostSetupFlow(ask, &out, func() error { r.Out = SetupOutput(); return modelFlowPluginProvider(r, profile.Name) }); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || strings.Join(commands[0], " ") != "proc-cli auth login" {
		t.Fatalf("commands %v\n%s", commands, out.String())
	}
	model := readConfig(t, h)["model"].(map[string]any)
	if model["provider"] != "proc-provider" || model["default"] != "pinned-a" || !strings.Contains(out.String(), "Proc Provider credentials: ✓") {
		t.Fatalf("model %v\n%s", model, out.String())
	}
}
