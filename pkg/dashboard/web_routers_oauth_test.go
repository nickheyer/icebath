package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Session client over a launch home that holds one named profile with its identity marker
func oauthProfileClient(t *testing.T, profile string) (sessionClient, string) {
	t.Helper()
	c := newSessionClient(t)
	profileHome := filepath.Join(os.Getenv("ICEBATH_HOME"), "profiles", profile)
	writeTestFile(t, filepath.Join(profileHome, "config.yaml"), "{}\n")
	return c, profileHome
}

// Replaces the MiniMax user code request, PKCE pair and poller of the start route for one case
func fakeMinimaxStart(t *testing.T, poller func(ctx context.Context, sessionID string)) {
	t.Helper()
	savedRequest, savedPair, savedPoller := oauthMinimaxRequestUserCode, oauthMinimaxPKCEPair, minimaxPoller
	oauthMinimaxRequestUserCode = func(context.Context, *http.Client, string, string, string, string) (map[string]any, error) {
		return map[string]any{"user_code": "ABCD-1234", "verification_uri": "https://api.minimax.io/oauth/verify",
			"expired_in": float64(600), "interval": float64(2000), "state": "fake-state"}, nil
	}
	oauthMinimaxPKCEPair = func() (string, string, string) { return "verifier-fake", "challenge-fake", "fake-state" }
	minimaxPoller = poller
	t.Cleanup(func() {
		oauthMinimaxRequestUserCode, oauthMinimaxPKCEPair, minimaxPoller = savedRequest, savedPair, savedPoller
	})
}

func dropTestOAuthSession(sessionID string) {
	oauthSessionsMu.Lock()
	delete(oauthSessions, sessionID)
	oauthSessionsMu.Unlock()
}

func oauthSessionIDs() []string {
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	ids := make([]string, 0, len(oauthSessions))
	for id := range oauthSessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestMinimaxLoginDoesNotLaunchAnthropicFlow(t *testing.T) {
	c := newSessionClient(t)
	fakeMinimaxStart(t, func(context.Context, string) {})
	resp := c.send("POST", "/api/providers/oauth/minimax-oauth/start", nil)
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	body := jsonObject(t, resp)
	defer dropTestOAuthSession(body["session_id"].(string))
	if _, has := body["auth_url"]; has || strings.Contains(strings.ToLower(string(resp.Body)), "claude.ai") {
		t.Fatalf("body %s", resp.Body)
	}
	if body["flow"] != "device_code" || !strings.Contains(strings.ToLower(body["verification_url"].(string)), "minimax") ||
		body["user_code"] != "ABCD-1234" || body["expires_in"] != int64(600) {
		t.Fatalf("body %s", resp.Body)
	}
}

func TestMinimaxStartRouteHonorsPollerMockOnOwningModule(t *testing.T) {
	c := newSessionClient(t)
	mockRan := make(chan struct{})
	var once sync.Once
	fakeMinimaxStart(t, func(context.Context, string) { once.Do(func() { close(mockRan) }) })
	realNetworkHit := make(chan struct{}, 1)
	savedPoll := oauthMinimaxPollToken
	oauthMinimaxPollToken = func(context.Context, *http.Client, string, string, string, string, int64, *int) (map[string]any, error) {
		realNetworkHit <- struct{}{}
		return nil, errors.New("real poller body must not run under the mock")
	}
	t.Cleanup(func() { oauthMinimaxPollToken = savedPoll })
	resp := c.send("POST", "/api/providers/oauth/minimax-oauth/start", nil)
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	defer dropTestOAuthSession(jsonObject(t, resp)["session_id"].(string))
	select {
	case <-mockRan:
	case <-time.After(5 * time.Second):
		t.Fatal("patched poller never ran: the router bypassed the seam")
	}
	select {
	case <-realNetworkHit:
		t.Fatal("the real poller body ran")
	default:
	}
}

func TestOAuthProviderStatusUsesProfileQuery(t *testing.T) {
	c, profileHome := oauthProfileClient(t, "coder")
	var observed []string
	saved := oauthProviderCatalog
	oauthProviderCatalog = []oauthCatalogEntry{{id: "fake-oauth", name: "Fake OAuth", flow: "pkce", cliCommand: "icebath auth add fake-oauth",
		docsURL: "https://example.com", statusFn: func(ctx context.Context) (*pyjson.Object, error) {
			observed = append(observed, home.Dir(ctx))
			return pyjson.ObjectOf("logged_in", false, "source", nil), nil
		}}}
	t.Cleanup(func() { oauthProviderCatalog = saved })
	if resp := c.get("/api/providers/oauth?profile=coder"); resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if !slices.Equal(observed, []string{profileHome}) {
		t.Fatalf("status read in %v, want %s", observed, profileHome)
	}
}

func TestOAuthStartStoresProfileForBackgroundCompletion(t *testing.T) {
	c, _ := oauthProfileClient(t, "coder")
	fakeMinimaxStart(t, func(context.Context, string) {})
	resp := c.send("POST", "/api/providers/oauth/minimax-oauth/start?profile=coder", nil)
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	sessionID := jsonObject(t, resp)["session_id"].(string)
	defer dropTestOAuthSession(sessionID)
	oauthSessionsMu.Lock()
	profile := oauthSessions[sessionID]["profile"]
	oauthSessionsMu.Unlock()
	if profile != "coder" {
		t.Fatalf("session profile %v", profile)
	}
}

func TestOAuthSessionCannotBePolledOrCancelledFromAnotherProfile(t *testing.T) {
	c, _ := oauthProfileClient(t, "worker")
	sessionID, _ := newOAuthSession("xai-oauth", "device_code", "worker")
	defer dropTestOAuthSession(sessionID)
	poll := c.get("/api/providers/oauth/xai-oauth/poll/" + sessionID)
	if poll.Status != 400 || !strings.Contains(strings.ToLower(string(poll.Body)), "profile") {
		t.Fatalf("poll from the default profile %d %s", poll.Status, poll.Body)
	}
	cancel := c.send("DELETE", "/api/providers/oauth/sessions/"+sessionID, nil)
	if cancel.Status != 400 || !strings.Contains(strings.ToLower(string(cancel.Body)), "profile") {
		t.Fatalf("cancel from the default profile %d %s", cancel.Status, cancel.Body)
	}
	if !slices.Contains(oauthSessionIDs(), sessionID) {
		t.Fatal("the refused cancel dropped the session")
	}
	if r := c.get("/api/providers/oauth/xai-oauth/poll/" + sessionID + "?profile=worker"); r.Status != 200 {
		t.Fatalf("poll from its profile %d %s", r.Status, r.Body)
	}
	if r := c.send("DELETE", "/api/providers/oauth/sessions/"+sessionID+"?profile=worker", nil); r.Status != 200 {
		t.Fatalf("cancel from its profile %d %s", r.Status, r.Body)
	}
}

// Providers of the accounts list keyed by id
func oauthProvidersByID(t *testing.T, c sessionClient) map[string]map[string]any {
	t.Helper()
	resp := c.get("/api/providers/oauth")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	out := map[string]map[string]any{}
	for _, p := range jsonObject(t, resp)["providers"].([]any) {
		row := p.(map[string]any)
		out[row["id"].(string)] = row
	}
	return out
}

func TestAnthropicDashboardOAuthIsRemovedAndExternal(t *testing.T) {
	c := newSessionClient(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	if flow := oauthProvidersByID(t, c)["anthropic"]["flow"]; flow != "external" {
		t.Fatalf("anthropic flow %v", flow)
	}
	before := oauthSessionIDs()
	start := c.send("POST", "/api/providers/oauth/anthropic/start", nil)
	if start.Status != 400 || strings.Contains(string(start.Body), "claude.ai") {
		t.Fatalf("start %d %s", start.Status, start.Body)
	}
	submit := c.send("POST", "/api/providers/oauth/anthropic/submit", map[string]any{"session_id": "unused", "code": "unused"})
	if submit.Status != 400 {
		t.Fatalf("submit %d %s", submit.Status, submit.Body)
	}
	if !slices.Equal(oauthSessionIDs(), before) {
		t.Fatal("a refused start registered a session")
	}
}

func TestAccountsOffersEveryOAuthProviderFromCatalog(t *testing.T) {
	c := newSessionClient(t)
	offered := oauthProvidersByID(t, c)
	for _, d := range provider.ProviderCatalog() {
		if _, ok := offered[d.Slug]; d.Tab == "accounts" && !ok {
			t.Fatalf("%s is an accounts provider of the model picker missing from /api/providers/oauth", d.Slug)
		}
	}
}

func TestExternalOAuthDisconnectRejectedBeforeAuthMutation(t *testing.T) {
	c := newSessionClient(t)
	saved := oauthClearProviderAuth
	oauthClearProviderAuth = func(provider.PoolEnv, string) (bool, error) {
		t.Error("external providers must not reach clear_provider_auth")
		return false, nil
	}
	t.Cleanup(func() { oauthClearProviderAuth = saved })
	if resp := c.send("DELETE", "/api/providers/oauth/qwen-oauth", nil); resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestEnvSourcedOAuthStatusIsNotDisconnectable(t *testing.T) {
	c := newSessionClient(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	anthropic := oauthProvidersByID(t, c)["anthropic"]
	if anthropic["status"].(map[string]any)["source"] != "env_var" || anthropic["disconnectable"] != false {
		t.Fatalf("anthropic card %v", anthropic)
	}
	if resp := c.send("DELETE", "/api/providers/oauth/anthropic", nil); resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestStatusFallsThroughToGenericDispatcherForCatalogOnlyProvider(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := oauthGetAuthStatus
	oauthGetAuthStatus = func(provider.PoolEnv, string) map[string]any {
		return map[string]any{"logged_in": true, "provider": "some-future-oauth", "name": "Future OAuth Provider",
			"access_token": "sk-future-secret-token-xyz", "expires_at": "2026-12-01T00:00:00Z", "has_refresh_token": true}
	}
	t.Cleanup(func() { oauthGetAuthStatus = saved })
	out := resolveProviderStatus(context.Background(), "some-future-oauth", nil)
	preview, _ := out.GetOr("token_preview", "").(string)
	if out.GetOr("logged_in", nil) != true || out.GetOr("source", nil) != "some-future-oauth" ||
		out.GetOr("source_label", nil) != "Future OAuth Provider" || preview == "" || strings.Contains(preview, "sk-future-secret-token-xyz") ||
		out.GetOr("expires_at", nil) != "2026-12-01T00:00:00Z" || out.GetOr("has_refresh_token", nil) != true {
		t.Fatalf("card %v", out)
	}
}

func TestQwenStatusCardCarriesTheGetterExpiryTokenAndAuthFile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	const expiresAtMS = 1_790_541_000_123
	saved := oauthGetQwenAuthStatus
	oauthGetQwenAuthStatus = func(provider.PoolEnv) map[string]any {
		return map[string]any{"logged_in": true, "auth_file": "/home/u/.qwen/oauth_creds.json", "source": "qwen-cli",
			"api_key": "qwen-access-token-secret-xyz", "expires_at_ms": int64(expiresAtMS)}
	}
	t.Cleanup(func() { oauthGetQwenAuthStatus = saved })
	out := resolveProviderStatus(context.Background(), "qwen-oauth", nil)
	if out.GetOr("logged_in", nil) != true {
		t.Fatalf("card %v", out)
	}
	text, isText := out.GetOr("expires_at", nil).(string)
	if !isText {
		t.Fatalf("expires_at %v is no string", out.GetOr("expires_at", nil))
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || expiresAt.UnixMilli() != expiresAtMS {
		t.Fatalf("expires_at %q %v", text, err)
	}
	preview, _ := out.GetOr("token_preview", "").(string)
	if out.GetOr("source_label", nil) != "/home/u/.qwen/oauth_creds.json" || preview == "" || strings.Contains(preview, "qwen-access-token-secret-xyz") {
		t.Fatalf("card %v", out)
	}
}
