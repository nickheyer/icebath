package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type auxOwnedKeyClient struct{ key string }

func (c *auxOwnedKeyClient) BaseURL() string { return "https://api.anthropic.com" }
func (c *auxOwnedKeyClient) APIKey() string  { return c.key }
func (c *auxOwnedKeyClient) Close() error    { return nil }
func (c *auxOwnedKeyClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, nil
}

// Seeds an expired borrowed Claude Code login and an owned pool grant under one temp home and token endpoint
func auxSeedOwnedAnthropic(t *testing.T) (context.Context, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ICEBATH_HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, name := range []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "")
	}
	borrowed := filepath.Join(dir, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(borrowed), 0o755); err != nil {
		t.Fatal(err)
	}
	creds, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "borrowed-token", "refreshToken": "borrowed-refresh", "expiresAt": 1}})
	if err := os.WriteFile(borrowed, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"credential_pool": map[string]any{"anthropic": []any{map[string]any{
		"id": "owned", "source": "manual:hermes_pkce", "auth_type": "oauth",
		"access_token": "owned-token", "refresh_token": "owned-refresh",
		"expires_at": time.Now().UnixMilli() + 3600000, "priority": 0}}}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	var refreshed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		token := ""
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			token, _ = body["refresh_token"].(string)
		} else if form, err := url.ParseQuery(string(raw)); err == nil {
			token = form.Get("refresh_token")
		}
		refreshed = append(refreshed, token)
		if token != "owned-refresh" {
			t.Errorf("refresh grant %q was spent, only the owned grant may refresh", token)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"owned-new-token","refresh_token":"owned-new-refresh","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	orig := anthropicOAuthTokenURLs
	anthropicOAuthTokenURLs = []string{srv.URL}
	t.Cleanup(func() { anthropicOAuthTokenURLs = orig })
	return context.Background(), borrowed, &refreshed
}

func TestAnthropicOwnedRouting(t *testing.T) {
	t.Run("test_owned_pool_precedes_expired_borrowed_login", func(t *testing.T) {
		ctx, borrowed, refreshed := auxSeedOwnedAnthropic(t)
		before, _ := os.ReadFile(borrowed)
		if got := ResolveAnthropicToken(modelsPoolEnv(ctx), ""); got != "owned-token" {
			t.Fatalf("resolved %q, the owned pool grant must win over the expired borrowed login", got)
		}
		after, _ := os.ReadFile(borrowed)
		if !bytes.Equal(before, after) || len(*refreshed) != 0 {
			t.Fatalf("borrowed login touched, refresh calls %v", *refreshed)
		}
	})
	t.Run("test_auxiliary_owned_refresh_does_not_spend_borrowed_rotation", func(t *testing.T) {
		ctx, borrowed, refreshed := auxSeedOwnedAnthropic(t)
		before, _ := os.ReadFile(borrowed)
		authErr := &AuxSDKError{Class: "AuthenticationError", Message: "Invalid API key", StatusCode: 401}
		route := auxLadderRoute{Client: &auxOwnedKeyClient{key: "owned-token"}, Task: "compression", ResolvedProvider: "anthropic",
			BaseInfo: "https://api.anthropic.com", ResolvedModel: "fixture", FinalModel: "fixture"}
		var steps []auxLadderStep
		yield := func(step auxLadderStep) (any, error) {
			steps = append(steps, step)
			return map[string]any{"choices": []any{}}, nil
		}
		if _, _, raised := auxLadderCredentialRungs(ctx, yield, authErr, route, map[string]any{}, false); raised != nil {
			t.Fatal(raised)
		}
		if len(steps) == 0 || steps[0].Kind != "retry_same_provider" {
			t.Fatalf("first ladder step %v, want retry_same_provider", steps)
		}
		after, _ := os.ReadFile(borrowed)
		if !bytes.Equal(before, after) {
			t.Fatal("the borrowed Claude Code login was rewritten")
		}
		if len(*refreshed) != 1 || (*refreshed)[0] != "owned-refresh" {
			t.Fatalf("refresh calls %v, want the owned grant once", *refreshed)
		}
		if ok, err := auxRefreshAnthropicCredentials(ctx, "unrelated-api-key"); ok || err != nil {
			t.Fatalf("an unrelated key refreshed %v %v", ok, err)
		}
	})
}
