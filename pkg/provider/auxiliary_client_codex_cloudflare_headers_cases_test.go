package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func auxB64URL(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// Syntactically valid Codex style JWT carrying the account id and optional residency claims
func auxMakeCodexJWT(accountID, dataResidency, computeResidency string) string {
	auth := map[string]any{"chatgpt_account_id": accountID, "chatgpt_plan_type": "plus"}
	if dataResidency != "" {
		auth["chatgpt_data_residency"] = dataResidency
	}
	if computeResidency != "" {
		auth["chatgpt_compute_residency"] = computeResidency
	}
	payload, _ := json.Marshal(map[string]any{"sub": "user-xyz", "exp": 9999999999, "https://api.openai.com/auth": auth})
	return auxB64URL([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + auxB64URL(payload) + "." + auxB64URL([]byte("fake-sig"))
}

// Default headers of the OpenAI SDK client inside a built auxiliary client, unwrapping the Codex adapter
func auxCodexClientDefaultHeaders(t *testing.T, client AuxClient) map[string]string {
	t.Helper()
	if codex, ok := client.(*CodexAuxiliaryClient); ok {
		client, _ = codex.realClient.(AuxClient)
	}
	return auxClientDefaultHeaders(t, client)
}

func auxAssertCodexIdentity(t *testing.T, headers map[string]string, accountID string) {
	t.Helper()
	if headers["originator"] != "icebath-agent" || headers["ChatGPT-Account-ID"] != accountID || headers["User-Agent"] != "IcebathAgent/"+Version {
		t.Fatalf("codex headers %v", headers)
	}
}

// Home whose Codex auth store holds one singleton token and no usable pool entry
func auxCodexSingletonHome(t *testing.T, token string) context.Context {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	writeCodexStore(t, home.Dir(ctx), map[string]any{"access_token": token, "refresh_token": "rt"}, time.Now().UTC().Format(time.RFC3339), nil)
	prev := auxSelectPoolEntryFn
	t.Cleanup(func() { auxSelectPoolEntryFn = prev })
	auxSelectPoolEntryFn = func(context.Context, string) (bool, *PooledCredential) { return false, nil }
	return ctx
}

func TestCodexCloudflareHeadersUserAgentAdvertisesHermesVersion(t *testing.T) {
	headers := codexCloudflareHeaders(auxMakeCodexJWT("acct-test-123", "", ""), CodexBaseURL)
	if headers["User-Agent"] != "IcebathAgent/"+Version || headers["originator"] != "icebath-agent" {
		t.Fatalf("headers %v", headers)
	}
}

func TestCodexCloudflareHeadersJwtWithoutChatgptAccountIdClaim(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"sub": "user-xyz", "exp": 9999999999})
	token := auxB64URL([]byte("{}")) + "." + auxB64URL(payload) + "." + auxB64URL([]byte("sig"))
	headers := codexCloudflareHeaders(token, CodexBaseURL)
	if headers["originator"] != "icebath-agent" {
		t.Fatalf("headers %v", headers)
	}
	if _, ok := headers["ChatGPT-Account-ID"]; ok {
		t.Fatalf("account id invented %v", headers)
	}
}

func TestCodexCloudflareHeadersResidencyHeaderFromJwtClaims(t *testing.T) {
	both := auxMakeCodexJWT("acct-test-123", "us", "eu")
	if got := codexCloudflareHeaders(both, CodexBaseURL)["x-openai-internal-codex-residency"]; got != "us" {
		t.Fatalf("data residency must win, got %q", got)
	}
	computeOnly := auxMakeCodexJWT("acct-test-123", "", "eu")
	var sent []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.5", "visibility": "list"}}})
	}))
	t.Cleanup(srv.Close)
	ctx := auxMainTestHome(t, "")
	fetchCodexModelsFromAPI(ctx, computeOnly, srv.URL)
	clear(codexOAuthContextCache)
	fetchCodexOAuthContextLengthsWithSource(ctx, computeOnly, srv.URL)
	if len(sent) != 2 {
		t.Fatalf("%d catalog probes reached the wire", len(sent))
	}
	for _, h := range sent {
		if h.Get("x-openai-internal-codex-residency") != "eu" || h.Get("ChatGPT-Account-ID") != "acct-test-123" {
			t.Fatalf("probe headers %v", h)
		}
	}
}

func TestCodexCloudflareHeadersNoResidencyClaimOmitsHeader(t *testing.T) {
	for _, token := range []string{auxMakeCodexJWT("acct-test-123", "", ""), "not-a-jwt", "", "only.one", "  ", "...."} {
		headers := codexCloudflareHeaders(token, CodexBaseURL)
		if _, ok := headers["x-openai-internal-codex-residency"]; ok || headers["originator"] != "icebath-agent" {
			t.Fatalf("%q headers %v", token, headers)
		}
	}
}

func TestAuxiliaryClientWiringBuildCodexClientPassesCodexHeaders(t *testing.T) {
	token := auxMakeCodexJWT("acct-aux-try-codex", "", "")
	ctx := auxCodexSingletonHome(t, token)
	client, _, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || client == nil {
		t.Fatalf("codex client %v %v", client, err)
	}
	auxAssertCodexIdentity(t, auxCodexClientDefaultHeaders(t, client), "acct-aux-try-codex")
}

func TestAuxiliaryClientWiringResolveProviderClientRawCodexPassesCodexHeaders(t *testing.T) {
	token := auxMakeCodexJWT("acct-aux-raw-codex", "", "")
	ctx := auxCodexSingletonHome(t, token)
	client, _, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.4", ProviderClientOptions{RawCodex: true})
	if err != nil || client == nil {
		t.Fatalf("raw codex client %v %v", client, err)
	}
	auxAssertCodexIdentity(t, auxCodexClientDefaultHeaders(t, client), "acct-aux-raw-codex")
}
