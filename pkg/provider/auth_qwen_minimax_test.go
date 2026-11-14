package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeQwenCreds(t *testing.T, tokens map[string]any) string {
	t.Helper()
	path := qwenCLIAuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tokens)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQwenResolveRuntimeCredentialsFreshToken(t *testing.T) {
	h := newTestHome(t)
	path := writeQwenCreds(t, map[string]any{"access_token": "qwen-access", "refresh_token": "qr", "expiry_date": time.Now().UnixMilli() + 3600_000})
	creds, err := resolveQwenRuntimeCredentials(testEnv(t, h), true)
	if err != nil || creds["api_key"] != "qwen-access" || creds["source"] != "qwen-cli" || creds["auth_file"] != path || creds["base_url"] != DefaultQwenBaseURL {
		t.Fatalf("%v %v", creds, err)
	}
}

func TestQwenResolveRuntimeCredentialsMissingAccessToken(t *testing.T) {
	h := newTestHome(t)
	writeQwenCreds(t, map[string]any{"refresh_token": "qr", "expiry_date": time.Now().UnixMilli() + 3600_000})
	_, err := resolveQwenRuntimeCredentials(testEnv(t, h), true)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "qwen_access_token_missing" {
		t.Fatalf("%v", err)
	}
	os.Remove(qwenCLIAuthPath())
	if _, err = resolveQwenRuntimeCredentials(testEnv(t, h), true); !errors.As(err, &ae) || ae.Code != "qwen_auth_missing" {
		t.Fatalf("%v", err)
	}
}

func TestQwenAuthStatusLoggedInAndRefreshesExpiredToken(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeQwenCreds(t, map[string]any{"access_token": "live", "refresh_token": "qr", "expiry_date": time.Now().UnixMilli() + 3600_000})
	if status := getQwenAuthStatus(env); status["logged_in"] != true || status["api_key"] != "live" {
		t.Fatalf("%v", status)
	}
	writeQwenCreds(t, map[string]any{"access_token": "stale", "refresh_token": "qr", "expiry_date": 1})
	scriptClient(t, &qwenHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		if f := readForm(t, req); f["refresh_token"] != "qr" || f["client_id"] != QwenOAuthClientID {
			t.Errorf("%v", f)
		}
		return textResponse(200, `{"access_token":"renewed","expires_in":7200}`), nil
	})
	status := getQwenAuthStatus(env)
	if status["logged_in"] != true || status["api_key"] != "renewed" {
		t.Fatalf("%v", status)
	}
	saved, _ := readQwenCLITokens()
	if saved["refresh_token"] != "qr" || saved["token_type"] != "Bearer" || saved["resource_url"] != "portal.qwen.ai" {
		t.Fatalf("%v", saved)
	}
	writeQwenCreds(t, map[string]any{"access_token": "stale", "refresh_token": "qr", "expiry_date": 1})
	scriptClient(t, &qwenHTTPClientFn, func(*http.Request) (*http.Response, error) { return textResponse(401, "bad"), nil })
	status = getQwenAuthStatus(env)
	if status["logged_in"] != false || !strings.Contains(anyString(status["error"]), "Re-run 'qwen auth qwen-oauth'. Response: bad") {
		t.Fatalf("%v", status)
	}
}

func TestMinimaxResolveTokenExpiryUnixTTLSeconds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if got := minimaxResolveTokenExpiryUnix(900, now); got != 1_700_000_900 {
		t.Fatal(got)
	}
	if got := minimaxResolveTokenExpiryUnix(1_700_000_900_000, now); got != 1_700_000_900 {
		t.Fatal(got)
	}
}

func TestMinimaxPKCEPairProducesValidS256(t *testing.T) {
	verifier, challenge, state := minimaxPKCEPair()
	if verifier == "" || state == "" || challenge != pkceCodeChallenge(verifier) {
		t.Fatal(verifier, challenge)
	}
}

func TestMinimaxRequestUserCodeStateMismatchRaises(t *testing.T) {
	scriptClient(t, &minimaxHTTPClientFn, func(*http.Request) (*http.Response, error) {
		return textResponse(200, `{"user_code":"u","verification_uri":"v","expired_in":60,"state":"other"}`), nil
	})
	_, err := minimaxRequestUserCode(t.Context(), minimaxHTTPClientFn(time.Second), "https://p", "c", "ch", "mine")
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "state_mismatch" {
		t.Fatalf("%v", err)
	}
}

func minimaxState(expiresIn time.Duration) map[string]any {
	return map[string]any{"access_token": "old", "refresh_token": "r0", "portal_base_url": "https://portal.test", "client_id": "cid",
		"inference_base_url": "https://api.minimax.io/anthropic/", "expires_at": epochToISO(nowEpoch() + expiresIn.Seconds())}
}

func TestMinimaxRefreshSkipsWhenNotExpiredAndRotatesNearExpiry(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"minimax-oauth": minimaxState(time.Hour)}})
	calls := 0
	var mu sync.Mutex
	scriptClient(t, &minimaxHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return textResponse(200, `{"status":"success","access_token":"new","refresh_token":"r1","expired_in":900}`), nil
	})
	creds, err := resolveMinimaxOAuthRuntimeCredentials(env)
	if err != nil || creds["api_key"] != "old" || creds["base_url"] != "https://api.minimax.io/anthropic" || calls != 0 {
		t.Fatalf("%v %v %d", creds, err, calls)
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"minimax-oauth": minimaxState(10 * time.Second)}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := BuildMinimaxOAuthTokenProvider(env).Mint(t.Context()); err != nil || tok != "new" {
				t.Errorf("%s %v", tok, err)
			}
		}()
	}
	wg.Wait()
	data := readAuthStore(t, h)
	if calls != 1 || data["active_provider"] != "nous" || data["providers"].(map[string]any)["minimax-oauth"].(map[string]any)["refresh_token"] != "r1" {
		t.Fatalf("%d %v", calls, data)
	}
	if src := CredentialSourceOf(BuildMinimaxOAuthTokenProvider(env)); src != CredentialSourceMinimaxOAuth {
		t.Fatal(src)
	}
}

func TestMinimaxResolveQuarantinesDeadTokensOnTerminalRefreshFailure(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"minimax-oauth": minimaxState(0)}})
	long := `{"error":"invalid_grant","pad":"` + strings.Repeat("x", 20000) + `"}`
	scriptClient(t, &minimaxHTTPClientFn, func(*http.Request) (*http.Response, error) { return textResponse(400, long), nil })
	_, err := resolveMinimaxOAuthRuntimeCredentials(env)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "refresh_failed" || !ae.ReloginRequired || !strings.HasSuffix(ae.Message, "...[truncated]") ||
		len(ae.Message) > minimaxOAuthErrorBodyLimit+100 {
		t.Fatalf("%v", err)
	}
	state := readAuthStore(t, h)["providers"].(map[string]any)["minimax-oauth"].(map[string]any)
	if _, has := state["access_token"]; has || state["last_auth_error"] == nil {
		t.Fatalf("%v", state)
	}
	if _, err = resolveMinimaxOAuthRuntimeCredentials(env); !errors.As(err, &ae) || ae.Code != "not_logged_in" {
		t.Fatalf("%v", err)
	}
}

func TestMinimaxOAuthLoginSetsActiveProvider(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	prevSleep := minimaxSleepFn
	minimaxSleepFn = func(time.Duration) {}
	t.Cleanup(func() { minimaxSleepFn = prevSleep })
	polls := 0
	scriptClient(t, &minimaxHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		form := readForm(t, req)
		if strings.HasSuffix(req.URL.Path, "/oauth/code") {
			if req.URL.Host != "api.minimax.io" || form["code_challenge_method"] != "S256" || req.Header.Get("x-request-id") == "" {
				t.Errorf("%s %v", req.URL, form)
			}
			return textResponse(200, `{"user_code":"MM-1","verification_uri":"https://minimax/verify","expired_in":300,"interval":100,"state":"`+form["state"]+`"}`), nil
		}
		polls++
		if polls == 1 {
			return textResponse(200, `{"status":"pending"}`), nil
		}
		return textResponse(200, `{"status":"success","access_token":"at","refresh_token":"rt","expired_in":900,"notification_message":"hi"}`), nil
	})
	var out strings.Builder
	if err := LoginMinimaxOAuth(env, &out, MinimaxLoginArgs{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	data := readAuthStore(t, h)
	state := data["providers"].(map[string]any)["minimax-oauth"].(map[string]any)
	if data["active_provider"] != "minimax-oauth" || state["access_token"] != "at" || state["region"] != "global" || state["inference_base_url"] != MinimaxOAuthGlobalInference {
		t.Fatalf("%v", data)
	}
	if !strings.Contains(out.String(), "Note from MiniMax: hi") || !strings.Contains(out.String(), "MM-1") {
		t.Fatal(out.String())
	}
	if status := GetMinimaxOAuthAuthStatus(env); status["logged_in"] != true {
		t.Fatalf("%v", status)
	}
}
