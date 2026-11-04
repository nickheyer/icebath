package provider

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	CodexOAuthAuthorizeURL   = "https://auth.openai.com/oauth/authorize"
	CodexOAuthBrowserScope   = "openid profile email offline_access"
	CodexBrowserCallbackPort = 1455
	CodexBrowserCallbackPath = "/auth/callback"
	CodexBrowserCallbackWait = 300 * time.Second
	CodexBrowserPortBusyCode = "codex_browser_port_busy"
	codexBrowserCallbackHost = "127.0.0.1"
)

// Authorize endpoint and callback port the browser flow uses, swappable so tests run a local OpenAI
var (
	codexOAuthAuthorizeURL   = CodexOAuthAuthorizeURL
	codexBrowserCallbackPort = CodexBrowserCallbackPort
)

func codexBrowserPortBusyNotice() string {
	return fmt.Sprintf("Port %d is already in use (a Codex CLI sign-in may be running). OpenAI only redirects to that port, so falling back to the device-code login.", CodexBrowserCallbackPort)
}

// CodexLoginFlows are the login flows auth.codex_login_flow may name
var CodexLoginFlows = []string{"device_code", "browser"}

// browser only when the user asked for it through the flag or auth.codex_login_flow
func codexLoginFlow(env PoolEnv, out io.Writer, args CodexLoginArgs) string {
	if args.Browser {
		return "browser"
	}
	authCfg, _ := env.config()["auth"].(map[string]any)
	flow := "device_code"
	if authCfg != nil {
		if v, has := authCfg["codex_login_flow"]; has {
			flow = anyString(v)
		}
	}
	flow = strings.ToLower(strings.TrimSpace(flow))
	if flow == "" {
		flow = "device_code"
	}
	if !containsString(CodexLoginFlows, flow) {
		fmt.Fprintf(out, "Ignoring unknown auth.codex_login_flow '%s' (expected one of %s).\n", flow, strings.Join(CodexLoginFlows, ", "))
		return "device_code"
	}
	return flow
}

// Device code login runner the browser flow falls back to, swappable so tests observe the fallback
var codexDeviceCodeLoginFn = codexDeviceCodeLogin

// Runs the Codex OAuth flow the args or config select, a busy callback port falling back to device code
func codexOAuthLogin(env PoolEnv, out io.Writer, args CodexLoginArgs) (map[string]any, error) {
	if codexLoginFlow(env, out, args) == "browser" {
		creds, err := codexBrowserLogin(env, out, !args.NoBrowser, args.Timeout)
		if err == nil {
			return creds, nil
		}
		var authErr *AuthError
		if !errors.As(err, &authErr) || authErr.Code != CodexBrowserPortBusyCode {
			return nil, err
		}
		fmt.Fprintln(out, codexBrowserPortBusyNotice())
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "Signing in to OpenAI Codex...")
	fmt.Fprintln(out, "(Icebath creates its own session — won't affect Codex CLI or VS Code)")
	fmt.Fprintln(out)
	return codexDeviceCodeLoginFn(env, out)
}

func codexBrowserAuthorizeURL(redirectURI, state, codeChallenge string) string {
	query := url.Values{
		"response_type": {"code"}, "client_id": {CodexOAuthClientID}, "redirect_uri": {redirectURI},
		"scope": {CodexOAuthBrowserScope}, "code_challenge": {codeChallenge}, "code_challenge_method": {"S256"},
		"id_token_add_organizations": {"true"}, "state": {state}}
	return codexOAuthAuthorizeURL + "?" + query.Encode()
}

// Swaps the authorization code for tokens at the endpoint the device flow also uses
func codexBrowserExchangeCode(env PoolEnv, code, redirectURI, codeVerifier string) (map[string]any, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {CodexOAuthClientID}, "code_verifier": {codeVerifier}}
	return codexTokenExchange(env.ctx(), form)
}

func codexBrowserState() string {
	var raw [32]byte
	fillRandom(raw[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// Authorization code plus PKCE login on the loopback listener, the port busy error letting the caller fall back
func codexBrowserLogin(env PoolEnv, out io.Writer, openBrowser bool, timeout time.Duration) (map[string]any, error) {
	codeVerifier := pkceCodeVerifier(64)
	state := codexBrowserState()
	handler, result := newLoopbackCallbackHandler(CodexBrowserCallbackPath, "OpenAI Codex")
	server, err := bindLoopbackCallbackServer(codexBrowserCallbackHost, codexBrowserCallbackPort, handler,
		func(message, code string) *AuthError { return codexErr(message, code, false) }, CodexBrowserPortBusyCode)
	if err != nil {
		return nil, err
	}
	redirectURI := fmt.Sprintf("http://localhost:%d%s", server.Port(), CodexBrowserCallbackPath)
	authURL := codexBrowserAuthorizeURL(redirectURI, state, pkceCodeChallenge(codeVerifier))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Signing in to OpenAI Codex (browser authorization)...")
	fmt.Fprintln(out, "(Icebath creates its own session — won't affect Codex CLI or VS Code)")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Open this URL to authorize Icebath:\n  %s\n\n", authURL)
	printLoopbackSSHHint(out, redirectURI, "")
	if openBrowser && canOpenGraphicalBrowser() {
		if openBrowserFn(authURL) == nil {
			fmt.Fprintln(out, "Browser opened for OpenAI authorization.")
		} else {
			fmt.Fprintln(out, "Could not open the browser automatically; use the URL above.")
		}
	}
	wait := timeout
	if wait <= 0 {
		wait = CodexBrowserCallbackWait
	}
	fmt.Fprintf(out, "Waiting for the OpenAI callback on %s (timeout %ds, Ctrl+C to cancel)...\n", redirectURI, int(wait.Seconds()))
	callback, err := serveLoopbackCallback(server, result, wait,
		func(message, code string) *AuthError { return codexErr(message, code, false) }, "codex_browser_callback_timeout")
	if err != nil {
		if env.ctx().Err() != nil {
			fmt.Fprintln(out, "\nLogin cancelled.")
			return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		return nil, err
	}
	if anyString(callback["error"]) != "" {
		detail := anyString(callback["error_description"])
		if detail == "" {
			detail = anyString(callback["error"])
		}
		return nil, codexErr("OpenAI authorization failed: "+detail, "codex_browser_auth_denied", false)
	}
	if subtle.ConstantTimeCompare([]byte(anyString(callback["state"])), []byte(state)) != 1 {
		return nil, codexErr("Authorization callback state mismatch — the redirect did not come from this login. Aborting.", "codex_browser_state_mismatch", false)
	}
	code := strings.TrimSpace(anyString(callback["code"]))
	if code == "" {
		return nil, codexErr("Authorization callback did not carry a code.", "codex_browser_no_code", false)
	}
	fmt.Fprintln(out, "Exchanging the authorization code for Codex tokens...")
	tokens, err := codexBrowserExchangeCode(env, code, redirectURI, codeVerifier)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"tokens":   map[string]any{"access_token": anyString(tokens["access_token"]), "refresh_token": anyString(tokens["refresh_token"])},
		"base_url": codexBaseURL(env), "last_refresh": utcNowZ(), "auth_mode": "chatgpt", "source": "loopback_pkce",
	}, nil
}
