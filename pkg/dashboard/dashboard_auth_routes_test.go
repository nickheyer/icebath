package dashboard

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Local fake IDP of the dashboard auth tests standing in for the conftest provider
type fakeIDPProvider struct{}

func (fakeIDPProvider) Name() string        { return "stub" }
func (fakeIDPProvider) DisplayName() string { return "Stub IdP (test only)" }
func (fakeIDPProvider) StartLogin(_ context.Context, redirectURI string) (gateway.LoginStart, error) {
	return gateway.LoginStart{RedirectURL: redirectURI + "?code=stub_code&state=s",
		CookiePayload: map[string]string{"icebath_session_pkce": "state=s;verifier=v"}}, nil
}
func (fakeIDPProvider) CompleteLogin(context.Context, string, string, string, string) (*gateway.DashboardSession, error) {
	return &gateway.DashboardSession{UserID: "u1", Provider: "stub"}, nil
}
func (fakeIDPProvider) VerifySession(context.Context, string) (*gateway.DashboardSession, error) {
	return nil, nil
}
func (fakeIDPProvider) RefreshSession(context.Context, string) (*gateway.DashboardSession, error) {
	return nil, nil
}
func (fakeIDPProvider) RevokeSession(context.Context, string) error { return nil }

func TestDistinctIPBucketsAreCapped(t *testing.T) {
	resetPasswordRateLimit()
	prev := pwRateMaxBuckets
	pwRateMaxBuckets = 3
	t.Cleanup(func() { pwRateMaxBuckets = prev; resetPasswordRateLimit() })
	for _, ip := range []string{"192.0.2.0", "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		if passwordRateLimited(ip) {
			t.Fatalf("%s limited", ip)
		}
	}
	if got := pwAttempts.Keys(); !slices.Equal(got, []string{"192.0.2.2", "192.0.2.3", "192.0.2.4"}) {
		t.Fatalf("buckets = %v", got)
	}
}

func TestExpiredBucketsPrunedBeforeEvictingLiveBucket(t *testing.T) {
	resetPasswordRateLimit()
	prev := pwRateMaxBuckets
	pwRateMaxBuckets = 2
	t.Cleanup(func() { pwRateMaxBuckets = prev; resetPasswordRateLimit() })
	now := pwMonotonic()
	pwAttempts.Set("expired", []float64{now - pwRateWindowSec - 1})
	pwAttempts.Set("live", []float64{now})
	if passwordRateLimited("192.0.2.99") {
		t.Fatal("limited")
	}
	if pwAttempts.Has("expired") || !pwAttempts.Has("live") || !pwAttempts.Has("192.0.2.99") {
		t.Fatalf("buckets = %v", pwAttempts.Keys())
	}
}

func TestClientIPUsesASGIPeerNotForwardedHeader(t *testing.T) {
	for _, peer := range []*Address{{Host: "203.0.113.7", Port: 12345}, nil} {
		req := NewRequest(&Scope{Type: "http", Client: peer,
			Headers: []Header{{"x-forwarded-for", "198.51.100.1, 192.0.2.1"}}}, nil, nil)
		want := ""
		if peer != nil {
			want = peer.Host
		}
		if got := clientIP(&req.HTTPConnection); got != want {
			t.Fatalf("client ip = %q, want %q", got, want)
		}
	}
}

func TestOauthOnlyPageStaysScriptFree(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gateway.ClearDashboardAuthProviders()
	t.Cleanup(gateway.ClearDashboardAuthProviders)
	if err := gateway.RegisterGlobalDashboardAuthProvider(fakeIDPProvider{}); err != nil {
		t.Fatal(err)
	}
	html, err := renderLoginHTML(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "provider-btn") || strings.Contains(html, "<script>") ||
		strings.Contains(html, `<form class="provider-form"`) || strings.Contains(html, "/auth/password-login") {
		t.Fatalf("page = %s", html)
	}
}
