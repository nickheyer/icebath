package messagetools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// RoundTripper calling a handler in process like httpx.MockTransport
type mockTransport func(req *http.Request) (*http.Response, error)

func (f mockTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Response with a status, a body and header pairs for a mock transport handler
func mockResponse(req *http.Request, status int, body string, headers ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Add(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req,
		ContentLength: int64(len(body))}
}

func TestGraphCredentialsFromEnvRaisesForMissingRequiredValues(t *testing.T) {
	_, err := GraphCredentialsFromEnv(map[string]string{}, true)
	var cfg *MicrosoftGraphConfigError
	if !errors.As(err, &cfg) {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"MSGRAPH_TENANT_ID", "MSGRAPH_CLIENT_ID", "MSGRAPH_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("%q missing from %q", name, err)
		}
	}
	var base *MicrosoftGraphAuthError
	if !errors.As(err, &base) || base.Error() != err.Error() {
		t.Fatalf("config error is not an auth error: %v", base)
	}
	if _, err := MicrosoftGraphTokenProviderFromEnv(map[string]string{"MSGRAPH_TENANT_ID": "t"}); !errors.As(err, &cfg) ||
		err.Error() != "Missing Microsoft Graph configuration: MSGRAPH_CLIENT_ID, MSGRAPH_CLIENT_SECRET" {
		t.Fatalf("provider from env err = %v", err)
	}
}

func TestGraphCredentialsFromEnvOptionalReturnsNilWhenNotConfigured(t *testing.T) {
	creds, err := GraphCredentialsFromEnv(map[string]string{}, false)
	if creds != nil || err != nil {
		t.Fatalf("creds = %v, err = %v", creds, err)
	}
}

func TestGraphCredentialsFromEnvBuildsNormalizedCredentials(t *testing.T) {
	creds, err := GraphCredentialsFromEnv(map[string]string{
		"MSGRAPH_TENANT_ID":     "tenant-123",
		"MSGRAPH_CLIENT_ID":     "client-456",
		"MSGRAPH_CLIENT_SECRET": "secret-789",
	}, true)
	if err != nil || creds == nil {
		t.Fatalf("creds = %v, err = %v", creds, err)
	}
	if creds.Scope != DefaultGraphScope {
		t.Fatalf("scope = %q", creds.Scope)
	}
	if !strings.HasSuffix(creds.TokenURL(), "/tenant-123/oauth2/v2.0/token") {
		t.Fatalf("token url = %q", creds.TokenURL())
	}
	t.Setenv("MSGRAPH_TENANT_ID", " /tenant-env/ ")
	t.Setenv("MSGRAPH_CLIENT_ID", "client")
	t.Setenv("MSGRAPH_CLIENT_SECRET", "secret")
	t.Setenv("MSGRAPH_AUTHORITY_URL", " https://login.example/ ")
	creds, err = GraphCredentialsFromEnv(nil, true)
	if err != nil || creds.TokenURL() != "https://login.example/tenant-env/oauth2/v2.0/token" {
		t.Fatalf("process env creds = %+v, err = %v", creds, err)
	}
}

func TestTokenProviderReusesCachedTokenUntilExpiry(t *testing.T) {
	var calls atomic.Int32
	var form string
	provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"), MicrosoftGraphTokenProviderOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			b, _ := io.ReadAll(req.Body)
			form = string(b)
			return mockResponse(req, 200, `{"access_token": "token-`+string(rune('0'+n))+`", "expires_in": 3600, "token_type": "Bearer"}`,
				"Content-Type", "application/json"), nil
		}),
	})
	first, err := provider.GetAccessToken(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.GetAccessToken(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if first != "token-1" || second != "token-1" || calls.Load() != 1 {
		t.Fatalf("first %q second %q calls %d", first, second, calls.Load())
	}
	if form != "grant_type=client_credentials&client_id=client&client_secret=secret&scope=https%3A%2F%2Fgraph.microsoft.com%2F.default" {
		t.Fatalf("form = %q", form)
	}
	health := provider.InspectTokenHealth()
	if v, _ := health.Get("cached"); v != true {
		t.Fatalf("health = %v", health.Keys)
	}
	if v, _ := health.Get("expires_in_seconds"); v.(int64) < 3590 {
		t.Fatalf("expires_in_seconds = %v", v)
	}
	forced, err := provider.GetAccessToken(context.Background(), true)
	if err != nil || forced != "token-2" {
		t.Fatalf("forced = %q, err = %v", forced, err)
	}
	provider.ClearCache()
	health = provider.InspectTokenHealth()
	text, _ := pyjson.Dumps(health)
	if text != `{"configured": true, "tenant_id": "tenant", "client_id": "client", "scope": "https://graph.microsoft.com/.default", `+
		`"authority_url": "https://login.microsoftonline.com", "token_url": "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", `+
		`"cached": false, "expires_in_seconds": null, "is_expired": null, "refresh_skew_seconds": 120}` {
		t.Fatalf("health = %s", text)
	}
}

func TestTokenProviderConcurrentCallsShareOneTokenFetch(t *testing.T) {
	var calls atomic.Int32
	provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"))
	release := make(chan struct{})
	provider.fetch = func(ctx context.Context) (*CachedAccessToken, error) {
		calls.Add(1)
		<-release
		return &CachedAccessToken{AccessToken: "token-1", TokenType: "Bearer", ExpiresAt: 9_999_999_999}, nil
	}
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := provider.GetAccessToken(context.Background(), false)
			if err != nil {
				t.Error(err)
			}
			got[i] = tok
		}()
	}
	close(release)
	wg.Wait()
	if got[0] != "token-1" || got[1] != "token-1" || calls.Load() != 1 {
		t.Fatalf("got %v calls %d", got, calls.Load())
	}
}

func TestTokenProviderHTTPErrorIncludesServerMessage(t *testing.T) {
	provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"), MicrosoftGraphTokenProviderOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			return mockResponse(req, 401, `{"error": "invalid_client", "error_description": "bad secret"}`), nil
		}),
	})
	_, err := provider.GetAccessToken(context.Background(), false)
	var tokErr *MicrosoftGraphTokenError
	if !errors.As(err, &tokErr) || !strings.Contains(err.Error(), "bad secret") {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "Microsoft Graph token request failed with HTTP 401: bad secret" {
		t.Fatalf("err = %q", err)
	}
}

func TestTokenResponseValidationAndErrorDetail(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error": {"code": "AADSTS7000215", "message": "Invalid client secret"}}`, "Microsoft Graph token request failed with HTTP 400: AADSTS7000215: Invalid client secret"},
		{400, `{"error": {"code": "OnlyCode"}}`, "Microsoft Graph token request failed with HTTP 400: OnlyCode"},
		{400, `{"other": 1}`, "Microsoft Graph token request failed with HTTP 400: {'other': 1}"},
		{500, `  upstream down  `, "Microsoft Graph token request failed with HTTP 500: upstream down"},
		{500, ``, "Microsoft Graph token request failed with HTTP 500: unknown error"},
		{502, `[1, 2]`, "Microsoft Graph token request failed with HTTP 502: [1, 2]"},
		{200, `not json`, "Microsoft Graph token response was not valid JSON."},
		{200, `{"expires_in": 10}`, "Microsoft Graph token response did not include access_token."},
		{200, `{"access_token": "  ", "expires_in": 10}`, "Microsoft Graph token response did not include access_token."},
		{200, `{"access_token": "tok"}`, "Microsoft Graph token response did not include a valid expires_in."},
		{200, `{"access_token": "tok", "expires_in": "soon"}`, "Microsoft Graph token response did not include a valid expires_in."},
		{200, `[]`, "'list' object has no attribute 'get'"},
	}
	for _, tc := range cases {
		provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"), MicrosoftGraphTokenProviderOptions{
			Transport: mockTransport(func(req *http.Request) (*http.Response, error) { return mockResponse(req, tc.status, tc.body), nil }),
		})
		_, err := provider.GetAccessToken(context.Background(), false)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
	provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"), MicrosoftGraphTokenProviderOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			return mockResponse(req, 200, `{"access_token": " tok ", "expires_in": "-5", "token_type": " "}`), nil
		}),
	})
	if tok, err := provider.GetAccessToken(context.Background(), false); err != nil || tok != "tok" {
		t.Fatalf("tok = %q, err = %v", tok, err)
	}
	cached := provider.cached.Load()
	if cached.TokenType != "Bearer" || !cached.IsExpired(0) || cached.ExpiresInSeconds() != 0 {
		t.Fatalf("cached = %+v", cached)
	}
}

func TestFormatGraphError(t *testing.T) {
	obj := func(kv ...any) *pyjson.Object {
		o := pyjson.NewObject()
		for i := 0; i < len(kv); i += 2 {
			o.Set(kv[i].(string), kv[i+1])
		}
		return o
	}
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{"bare", "bare", true},
		{obj("code", "Forbidden", "message", "nope"), "Forbidden: nope", true},
		{obj("message", "only message"), "only message", true},
		{obj("code", "OnlyCode"), "", false},
		{obj("code", int64(7), "message", int64(9)), "7: 9", true},
		{int64(3), "", false},
		{nil, "", false},
	}
	for _, tc := range cases {
		got, ok := FormatGraphError(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%v: got %q %v", tc.in, got, ok)
		}
	}
}
