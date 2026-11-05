package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const openrouterErrorBodyLimit = 2048

// Client for the OpenRouter code exchange, swappable so tests aim it at a local server
var openrouterHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Endpoint the OpenRouter code exchange posts to, swappable so tests aim it at a local server
var openrouterAuthKeysURL = OpenRouterAuthKeysURL

func openrouterErrFn(message, code string) *AuthError { return openrouterErr(message, code, false) }

// Exchanges the authorization code for an OpenRouter API key that never enters a log or error message
func openrouterExchangeCode(ctx context.Context, code, codeVerifier string, timeout time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": codeVerifier, "code_challenge_method": "S256"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openrouterAuthKeysURL, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var resp *oauthHTTPResponse
	if err == nil {
		resp, err = oauthDo(openrouterHTTPClientFn(timeout), req, 0, nil)
	}
	if err != nil {
		ae := openrouterErr(fmt.Sprintf("OpenRouter code exchange failed: %s", err), "openrouter_token_exchange_failed", false)
		ae.Err = err
		return "", ae
	}
	if resp.Status == 403 {
		return "", openrouterErr("OpenRouter rejected the authorization code (invalid, already used, or older than 10 minutes). "+
			"Run the login again.", "openrouter_token_exchange_denied", true)
	}
	if resp.Status >= 400 {
		detail := []rune(strings.TrimSpace(string(resp.Body)))
		if len(detail) > openrouterErrorBodyLimit {
			detail = detail[:openrouterErrorBodyLimit]
		}
		suffix := ""
		if len(detail) > 0 {
			suffix = " Response: " + string(detail)
		}
		return "", openrouterErr(fmt.Sprintf("OpenRouter code exchange failed (HTTP %d).%s", resp.Status, suffix), "openrouter_token_exchange_failed", false)
	}
	raw, err := resp.JSON()
	if err != nil {
		ae := openrouterErr("OpenRouter code exchange returned a non-JSON body.", "openrouter_token_exchange_invalid", false)
		ae.Err = err
		return "", ae
	}
	key := ""
	if payload, ok := raw.(map[string]any); ok {
		key = strings.TrimSpace(pyOrStr(payload["key"]))
	}
	if key == "" {
		return "", openrouterErr("OpenRouter code exchange response did not include a 'key'.", "openrouter_token_exchange_invalid", false)
	}
	return key, nil
}

// Remote session flow where OpenRouter shows the code on screen and the user pastes it
func openrouterHeadlessCode(out io.Writer, authURL string) (string, error) {
	fmt.Fprintf(out, "Remote session detected — using OpenRouter's headless flow.\n"+
		"Open this URL in a browser on any machine, authorize, then paste the code shown:\n  %s\n\n", authURL)
	code, err := maskedSecretPrompt("Authorization code: ")
	if err != nil {
		return "", err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", openrouterErr("No authorization code entered.", "openrouter_auth_no_code", false)
	}
	return code, nil
}

// Local flow that serves a nonce path on a loopback port and waits for OpenRouter's callback
func openrouterLoopbackCode(out io.Writer, params [][2]string, openBrowser bool, timeout time.Duration) (string, error) {
	path := "/callback/" + pkceTokenURLSafe(16)
	handler, result := newLoopbackCallbackHandler(path, "OpenRouter")
	server, err := bindLoopbackCallbackServer("127.0.0.1", 0, handler, openrouterErrFn, "openrouter_callback_bind_failed")
	if err != nil {
		return "", err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", server.Port(), path)
	authURL := OpenRouterAuthURL + "?" + orderedQuery(append([][2]string{{"callback_url", redirectURI}}, params...))
	fmt.Fprintf(out, "Open this URL to authorize Icebath with OpenRouter:\n  %s\n\nDocs: %s\n", authURL, OpenRouterOAuthDocsURL)
	if openBrowser && canOpenGraphicalBrowser() {
		if openBrowserFn(authURL) == nil {
			fmt.Fprintln(out, "Browser opened for OpenRouter authorization.")
		} else {
			fmt.Fprintln(out, "Could not open the browser automatically; use the URL above.")
		}
	}
	fmt.Fprintln(out, "Waiting for the OpenRouter callback...")
	callback, err := serveLoopbackCallback(server, result, timeout, openrouterErrFn, "openrouter_callback_timeout")
	if err != nil {
		return "", err
	}
	if pyTruthy(callback["error"]) {
		reason := pyOrStr(callback["error_description"])
		if reason == "" {
			reason = pyStr(callback["error"])
		}
		return "", openrouterErr("OpenRouter authorization failed: "+reason, "openrouter_auth_denied", false)
	}
	code := strings.TrimSpace(pyOrStr(callback["code"]))
	if code == "" {
		return "", openrouterErr("OpenRouter callback did not carry an authorization code.", "openrouter_auth_no_code", false)
	}
	return code, nil
}

// OpenrouterPKCELogin runs the OpenRouter PKCE flow and returns the minted API key for the pool add path
func OpenrouterPKCELogin(env PoolEnv, out io.Writer, openBrowser bool, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	codeVerifier := pkceCodeVerifier(64)
	params := [][2]string{{"code_challenge", pkceCodeChallenge(codeVerifier)}, {"code_challenge_method", "S256"}}
	var code string
	var err error
	if isRemoteSession() {
		code, err = openrouterHeadlessCode(out, OpenRouterAuthURL+"?"+orderedQuery(append(params, [2]string{"key_label", "icebath-agent"})))
	} else {
		code, err = openrouterLoopbackCode(out, params, openBrowser, timeout)
	}
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "Exchanging the authorization code for an OpenRouter API key...")
	key, err := openrouterExchangeCode(env.ctx(), code, codeVerifier, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]any{"api_key": key}, nil
}
