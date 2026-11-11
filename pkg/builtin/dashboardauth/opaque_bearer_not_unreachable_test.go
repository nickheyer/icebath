package dashboardauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	opaquePeerKey = "hk_live_opaque_peer_key_0123456789abcdef"
	foreignKidJWT = "eyJhbGciOiJSUzI1NiIsImtpZCI6Inp6eiJ9.e30.sig"
)

// Loopback JWKS server answering every GET with an empty key set
func emptyJWKSServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"keys": []}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func opaqueNous(t *testing.T, portalURL string) *NousDashboardAuthProvider {
	t.Helper()
	p, err := NewNousDashboardAuthProvider("agent:test-instance", portalURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// test_opaque_bearer_not_unreachable.py test_opaque_bearer_with_healthy_portal_is_not_unreachable
func TestOpaqueBearerWithHealthyPortalIsNotUnreachable(t *testing.T) {
	s, err := opaqueNous(t, emptyJWKSServer(t)).VerifySession(context.Background(), opaquePeerKey)
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_opaque_bearer_not_unreachable.py test_foreign_kid_jwt_with_healthy_portal_is_not_unreachable
func TestForeignKidJWTWithHealthyPortalIsNotUnreachable(t *testing.T) {
	s, err := opaqueNous(t, emptyJWKSServer(t)).VerifySession(context.Background(), foreignKidJWT)
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_opaque_bearer_not_unreachable.py test_real_jwt_with_unreachable_portal_still_raises_provider_error
func TestRealJWTWithUnreachablePortalStillRaisesProviderError(t *testing.T) {
	_, err := opaqueNous(t, "http://127.0.0.1:9").VerifySession(context.Background(), foreignKidJWT)
	var providerErr *gateway.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatal(err)
	}
}

// test_opaque_bearer_not_unreachable.py test_opaque_bearer_with_unreachable_portal_is_still_just_not_ours
func TestOpaqueBearerWithUnreachablePortalIsStillJustNotOurs(t *testing.T) {
	s, err := opaqueNous(t, "http://127.0.0.1:9").VerifySession(context.Background(), opaquePeerKey)
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_opaque_bearer_not_unreachable.py test_self_hosted_provider_shares_the_classification
func TestSelfHostedProviderSharesTheClassification(t *testing.T) {
	server := emptyJWKSServer(t)
	p, err := NewSelfHostedOIDCProvider(server, "hermes", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.discovery = &oidcDiscovery{jwksURI: server + "/jwks", issuer: server, authMethods: []string{}}
	p.discoveryFetchedAt = float64(time.Now().UnixNano()) / 1e9
	_, err = p.verifyIDToken(opaquePeerKey)
	var invalid *gateway.InvalidCodeError
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}
}
