package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type exchange func(*http.Request) (*http.Response, error)

func (e exchange) RoundTrip(request *http.Request) (*http.Response, error) {
	return e(request)
}

func reply(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func isolate(t *testing.T) (*credentials.Credentials, *config.Config, string) {
	t.Helper()
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	for _, variable := range []string{anthropicconfig.APIKeyEnv, anthropicconfig.TokenEnv, anthropicconfig.OAuthTokenEnv} {
		t.Setenv(variable, "")
	}
	cfg := config.New(t.TempDir())
	anthropicconfig.Register(cfg)
	host := credentials.New()
	Register(&host.Registry, cfg)
	return host, cfg, filepath.Join(claude, ".credentials.json")
}

func login(t *testing.T, path, access, refresh string, expires time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refresh, "expiresAt": expires.UnixMilli(), "scopes": []string{"user:inference"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func stored(t *testing.T, path string) (access, refresh string, expires int64, scopes []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Login struct {
			Access  string   `json:"accessToken"`
			Refresh string   `json:"refreshToken"`
			Expires int64    `json:"expiresAt"`
			Scopes  []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	return written.Login.Access, written.Login.Refresh, written.Login.Expires, written.Login.Scopes
}

func TestTokenShapes(t *testing.T) {
	for key, want := range map[string]bool{
		"":                      false,
		"sk-ant-api03-key":      false,
		"sk-ant-admin01-key":    false,
		"sk-ant-oat01-token":    true,
		"sk-ant-ort01-token":    true,
		"eyJhbGciOiJIUzI1NiJ9":  true,
		"cc-token":              true,
		"sk-or-v1-key":          false,
		"sk-ant-usr-something":  false,
		"sk-ant-svc-something":  false,
		"sk-ant-sid01-whatever": true,
	} {
		if got := IsAnthropicOAuthToken(key); got != want {
			t.Errorf("IsAnthropicOAuthToken(%q) = %v, want %v", key, got, want)
		}
	}
	mint := func(context.Context) (string, error) { return "sk-ant-oat01-minted", nil }
	fail := func(context.Context) (string, error) { return "", errors.New("no token") }
	for _, c := range []struct {
		base, credential, name string
		source                 func(context.Context) (string, error)
		want                   bool
	}{
		{"https://api.anthropic.com", "sk-ant-oat01-token", "", nil, true},
		{"", "sk-ant-oat01-token", "", nil, true},
		{"https://api.anthropic.com", "sk-ant-api03-key", "anthropic", nil, false},
		{"https://proxy.test", "sk-ant-oat01-token", "", nil, false},
		{"https://proxy.test", "sk-ant-oat01-token", " Anthropic ", nil, true},
		{"https://api.anthropic.com", "", "", mint, true},
		{"https://api.anthropic.com", "", "", fail, false},
	} {
		if got := AnthropicRouteIsOAuth(t.Context(), c.base, c.credential, c.source, c.name); got != c.want {
			t.Errorf("AnthropicRouteIsOAuth(%q, %q, %q) = %v, want %v", c.base, c.credential, c.name, got, c.want)
		}
	}
}

func TestNothingSetIsNoCredential(t *testing.T) {
	host, _, _ := isolate(t)
	if found, ok := host.Credential(anthropicconfig.Name); ok {
		t.Errorf("Credential() = %+v, want none", found)
	}
	t.Setenv(anthropicconfig.TokenEnv, "sk-ant-oat01-token")
	if found, ok := host.Credential("openrouter"); ok {
		t.Errorf("Credential(openrouter) = %+v, want none from the anthropic source", found)
	}
}

func TestTokenVariablesComeBeforeTheKeyVariable(t *testing.T) {
	host, _, _ := isolate(t)
	t.Setenv(anthropicconfig.APIKeyEnv, " sk-ant-api03-key ")
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-api03-key" || found.Auth != provider.AuthAPIKey || found.Source != anthropicconfig.APIKeyEnv {
		t.Errorf("Credential() with the key variable = %+v, %v", found, ok)
	}
	t.Setenv(anthropicconfig.OAuthTokenEnv, "sk-ant-oat01-code")
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-oat01-code" || found.Auth != provider.AuthOAuth || found.Source != anthropicconfig.OAuthTokenEnv {
		t.Errorf("Credential() with the Claude Code token variable = %+v, %v", found, ok)
	}
	t.Setenv(anthropicconfig.TokenEnv, "sk-ant-oat01-token")
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-oat01-token" || found.Auth != provider.AuthOAuth || found.Source != anthropicconfig.TokenEnv {
		t.Errorf("Credential() with the token variable = %+v, %v", found, ok)
	}
}

func TestConfiguredKeyStandsWhereItsVariableDoes(t *testing.T) {
	host, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-borrowed", "sk-ant-ort01-borrowed", time.Now().Add(time.Hour))
	if err := cfg.Set(anthropicconfig.APIKey, "sk-ant-api03-configured"); err != nil {
		t.Fatal(err)
	}
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-api03-configured" || found.Auth != provider.AuthAPIKey || found.Source != "config" {
		t.Errorf("Credential() = %+v, %v, want the configured key before the borrowed login", found, ok)
	}
	if err := cfg.Set(anthropicconfig.Token, "sk-ant-oat01-configured"); err != nil {
		t.Fatal(err)
	}
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-oat01-borrowed" || found.Source != "claude_code" {
		t.Errorf("Credential() = %+v, %v, want the refreshable login preferred over a static oauth token", found, ok)
	}
}

func TestClaudeCodeLoginIsBorrowedUnlessTurnedOff(t *testing.T) {
	host, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-live", "sk-ant-ort01-live", time.Now().Add(time.Hour))
	if found, ok := host.Credential(anthropicconfig.Name); !ok || found.APIKey != "sk-ant-oat01-live" || found.Auth != provider.AuthOAuth || found.Source != "claude_code" {
		t.Errorf("Credential() = %+v, %v, want the Claude Code login", found, ok)
	}
	if err := cfg.Set(anthropicconfig.AdoptExternalLogins, false); err != nil {
		t.Fatal(err)
	}
	if found, ok := host.Credential(anthropicconfig.Name); ok {
		t.Errorf("Credential() = %+v, want none with external logins not adopted", found)
	}
}

func TestExpiredLoginIsRefreshedAndWrittenBack(t *testing.T) {
	_, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-old", "sk-ant-ort01-old", time.Now().Add(-time.Hour))
	var target, agent, kind string
	var form url.Values
	env := PoolEnv{Config: cfg, Ctx: t.Context(), HTTP: &http.Client{Transport: exchange(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		target, agent, kind = request.URL.String(), request.Header.Get("User-Agent"), request.Header.Get("Content-Type")
		form, _ = url.ParseQuery(string(body))
		return reply(request, http.StatusOK, `{"access_token":"sk-ant-oat01-new","refresh_token":"sk-ant-ort01-new","expires_in":3600}`), nil
	})}}
	if token := ResolveAnthropicToken(env); token != "sk-ant-oat01-new" {
		t.Fatalf("ResolveAnthropicToken() = %q, want the refreshed token", token)
	}
	if target != "https://platform.claude.com/v1/oauth/token" || agent != "axios/1.7.9" || kind != "application/x-www-form-urlencoded" {
		t.Errorf("refresh request = %q %q %q", target, agent, kind)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "sk-ant-ort01-old" || form.Get("client_id") != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Errorf("refresh form = %v", form)
	}
	access, refresh, expires, scopes := stored(t, path)
	if access != "sk-ant-oat01-new" || refresh != "sk-ant-ort01-new" || expires <= time.Now().UnixMilli() || len(scopes) != 1 {
		t.Errorf("credentials file after the refresh = %q %q %d %v", access, refresh, expires, scopes)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %v, %v, want 0600", info.Mode(), err)
	}
}

func TestSecondTokenHostAnswersWhenTheFirstFails(t *testing.T) {
	_, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-stale", "sk-ant-ort01-stale", time.Now().Add(-time.Hour))
	var hosts []string
	env := PoolEnv{Config: cfg, HTTP: &http.Client{Transport: exchange(func(request *http.Request) (*http.Response, error) {
		hosts = append(hosts, request.URL.Host)
		if request.URL.Host == "platform.claude.com" {
			return reply(request, http.StatusBadGateway, `{"error":"upstream"}`), nil
		}
		return reply(request, http.StatusOK, `{"access_token":"sk-ant-oat01-fallback","expires_in":60}`), nil
	})}}
	if token := ResolveAnthropicToken(env); token != "sk-ant-oat01-fallback" {
		t.Fatalf("ResolveAnthropicToken() = %q, want the token of the fallback host", token)
	}
	if strings.Join(hosts, " ") != "platform.claude.com console.anthropic.com" {
		t.Errorf("token hosts = %v", hosts)
	}
	if _, refresh, _, _ := stored(t, path); refresh != "sk-ant-ort01-stale" {
		t.Errorf("refresh token = %q, want the old one kept when the reply carries none", refresh)
	}
}

func TestRejectedRefreshTokenIsNotReplayed(t *testing.T) {
	_, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-dead", "sk-ant-ort01-dead", time.Now().Add(-time.Hour))
	calls := 0
	env := PoolEnv{Config: cfg, HTTP: &http.Client{Transport: exchange(func(request *http.Request) (*http.Response, error) {
		calls++
		return reply(request, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`), nil
	})}}
	for range 2 {
		if token := ResolveAnthropicToken(env); token != "" {
			t.Errorf("ResolveAnthropicToken() = %q, want none after a dead grant", token)
		}
	}
	if calls != 1 {
		t.Errorf("token endpoint calls = %d, want the dead refresh token posted once", calls)
	}
}

func TestRotationThatCannotBeWrittenFailsClosed(t *testing.T) {
	_, cfg, path := isolate(t)
	login(t, path, "sk-ant-oat01-spent", "sk-ant-ort01-spent", time.Now().Add(-time.Hour))
	write := atomicWritePrivate
	atomicWritePrivate = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	t.Cleanup(func() { atomicWritePrivate = write })
	calls := 0
	env := PoolEnv{Config: cfg, HTTP: &http.Client{Transport: exchange(func(request *http.Request) (*http.Response, error) {
		calls++
		return reply(request, http.StatusOK, `{"access_token":"sk-ant-oat01-lost","refresh_token":"sk-ant-ort01-lost","expires_in":3600}`), nil
	})}}
	if token := ResolveAnthropicToken(env); token != "" {
		t.Fatalf("ResolveAnthropicToken() = %q, want none when the rotated pair was not committed", token)
	}
	atomicWritePrivate = write
	if token := ResolveAnthropicToken(env); token != "" || calls != 1 {
		t.Errorf("ResolveAnthropicToken() = %q after %d token calls, want the spent pair refused without a second post", token, calls)
	}
	raw, err := os.ReadFile(path + ".icebath-spent-rotations.json")
	if err != nil || !bytes.Contains(raw, []byte(FingerprintSecretValue("sk-ant-ort01-spent"))) || bytes.Contains(raw, []byte("sk-ant-ort01-spent")) {
		t.Errorf("spent rotation sidecar = %s, %v, want the fingerprint and never the token", raw, err)
	}
	if access, _, _, _ := stored(t, path); access != "sk-ant-oat01-spent" {
		t.Errorf("credentials file access token = %q, want it untouched", access)
	}
}

func TestSignInExchangesTheCodeItWasGiven(t *testing.T) {
	_, cfg, _ := isolate(t)
	var out bytes.Buffer
	var sent map[string]string
	env := PoolEnv{Config: cfg, HTTP: &http.Client{Transport: exchange(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
			t.Errorf("exchange body: %v", err)
		}
		return reply(request, http.StatusOK, `{"access_token":"sk-ant-oat01-signed","refresh_token":"sk-ant-ort01-signed","expires_in":28800}`), nil
	})}}
	link := regexp.MustCompile(`https://claude\.ai/oauth/authorize\?\S+`)
	state := func() url.Values {
		parsed, err := url.Parse(link.FindString(out.String()))
		if err != nil {
			t.Fatal(err)
		}
		return parsed.Query()
	}
	result := RunIcebathOAuthLoginTo(env, &out, func(string) (string, bool) { return " the-code#" + state().Get("state") + " ", true }, false)
	if result["access_token"] != "sk-ant-oat01-signed" || result["refresh_token"] != "sk-ant-ort01-signed" {
		t.Fatalf("RunIcebathOAuthLoginTo() = %v", result)
	}
	if expires, _ := result["expires_at_ms"].(float64); expires < float64(time.Now().Add(7*time.Hour).UnixMilli()) {
		t.Errorf("expires_at_ms = %v, want about eight hours from now", result["expires_at_ms"])
	}
	query := state()
	if query.Get("client_id") != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
		query.Get("redirect_uri") != "https://console.anthropic.com/oauth/code/callback" || query.Get("scope") != "org:create_api_key user:profile user:inference" {
		t.Errorf("authorize link = %v", query)
	}
	if sent["grant_type"] != "authorization_code" || sent["code"] != "the-code" || sent["state"] != query.Get("state") || sent["code_verifier"] == "" ||
		sent["redirect_uri"] != query.Get("redirect_uri") || sent["client_id"] != query.Get("client_id") {
		t.Errorf("exchange = %v", sent)
	}
	if again := RunIcebathOAuthLoginTo(env, io.Discard, func(string) (string, bool) { return "the-code#forged", true }, false); again != nil {
		t.Errorf("RunIcebathOAuthLoginTo() with a forged state = %v, want nothing", again)
	}
	if none := RunIcebathOAuthLoginTo(env, io.Discard, func(string) (string, bool) { return "", false }, false); none != nil {
		t.Errorf("RunIcebathOAuthLoginTo() when the prompt was dismissed = %v, want nothing", none)
	}
}

func TestSetupTokenNeedsTheClaudeCommand(t *testing.T) {
	_, cfg, _ := isolate(t)
	look := anthLookPath
	anthLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { anthLookPath = look })
	t.Setenv("HOME", t.TempDir())
	prefixes := anthClaudeCodePrefixes
	anthClaudeCodePrefixes = []string{"~/bin"}
	t.Cleanup(func() { anthClaudeCodePrefixes = prefixes })
	if token, err := RunOAuthSetupToken(PoolEnv{Config: cfg}); token != "" || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("RunOAuthSetupToken() = %q, %v, want a missing command error", token, err)
	}
}
