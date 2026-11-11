package dashboardauth

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const leewayPortal = "https://portal.example.com"

// Fixture key token with iat shifted by iatOffset seconds and the given lifetime
func mintSkewedToken(t *testing.T, iss, aud string, iatOffset, ttlSeconds int64) string {
	t.Helper()
	now := time.Now().Unix()
	return signRS256(t, oidcFixtureKey(), map[string]any{"iss": iss, "aud": aud, "sub": "usr_abc", "iat": now + iatOffset,
		"exp": now + ttlSeconds})
}

func leewaySelfHosted(t *testing.T, leeway any) *SelfHostedOIDCProvider {
	t.Helper()
	p, err := NewSelfHostedOIDCProvider(oidcIssuer, oidcClientID, "", "", leeway)
	if err != nil {
		t.Fatal(err)
	}
	p.discovery = &oidcDiscovery{issuer: oidcIssuer, authorizationEndpoint: oidcIssuer + "/authorize",
		tokenEndpoint: oidcIssuer + "/token", jwksURI: oidcIssuer + "/jwks", authMethods: []string{}}
	p.discoveryFetchedAt = float64(time.Now().UnixNano()) / 1e9
	p.setJWKSClient(makeJWKSClient(oidcJWKSURL(t)))
	return p
}

func leewayNous(t *testing.T) *NousDashboardAuthProvider {
	t.Helper()
	p, err := NewNousDashboardAuthProvider("agent:inst123", leewayPortal, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.setJWKSClient(makeJWKSClient(oidcJWKSURL(t)))
	return p
}

func sharedVerify(t *testing.T, token string) error {
	t.Helper()
	claims, err := verifyJWT(token, makeJWKSClient(oidcJWKSURL(t)), jwtCheck{algorithms: []string{"RS256"}, audience: oidcClientID,
		issuer: oidcIssuer, label: "ID token", leeway: DefaultTokenLeewaySeconds})
	if err == nil && claims.GetOr("sub", nil) != "usr_abc" {
		t.Fatal(claims)
	}
	return err
}

// test_jwt_clock_skew_leeway.py TestDefaultLeewayAbsorbsClockSkew test_shared_verify_jwt
func TestDefaultLeewayAbsorbsClockSkewSharedVerifyJWT(t *testing.T) {
	if err := sharedVerify(t, mintSkewedToken(t, oidcIssuer, oidcClientID, 30, 900)); err != nil {
		t.Fatal(err)
	}
}

// test_jwt_clock_skew_leeway.py TestDefaultLeewayAbsorbsClockSkew test_self_hosted_verify_session
func TestDefaultLeewayAbsorbsClockSkewSelfHostedVerifySession(t *testing.T) {
	s, err := leewaySelfHosted(t, nil).VerifySession(context.Background(), mintSkewedToken(t, oidcIssuer, oidcClientID, 30, 900))
	if err != nil || s == nil || s.UserID != "usr_abc" {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_jwt_clock_skew_leeway.py TestDefaultLeewayAbsorbsClockSkew test_nous_verify_session
func TestDefaultLeewayAbsorbsClockSkewNousVerifySession(t *testing.T) {
	s, err := leewayNous(t).VerifySession(context.Background(), mintSkewedToken(t, leewayPortal, "agent:inst123", 30, 900))
	if err != nil || s == nil || s.UserID != "usr_abc" {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_jwt_clock_skew_leeway.py TestImmatureSignatureClassification test_shared_verify_jwt_raises_invalid_code
func TestImmatureSignatureClassificationSharedVerifyJWTRaisesInvalidCode(t *testing.T) {
	err := sharedVerify(t, mintSkewedToken(t, oidcIssuer, oidcClientID, 3600, 900))
	var invalid *gateway.InvalidCodeError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "not yet valid") {
		t.Fatal(err)
	}
}

// test_jwt_clock_skew_leeway.py TestImmatureSignatureClassification test_self_hosted_verify_session_returns_none
func TestImmatureSignatureClassificationSelfHostedVerifySessionReturnsNone(t *testing.T) {
	s, err := leewaySelfHosted(t, nil).VerifySession(context.Background(), mintSkewedToken(t, oidcIssuer, oidcClientID, 3600, 900))
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_jwt_clock_skew_leeway.py TestImmatureSignatureClassification test_nous_verify_session_returns_none
func TestImmatureSignatureClassificationNousVerifySessionReturnsNone(t *testing.T) {
	s, err := leewayNous(t).VerifySession(context.Background(), mintSkewedToken(t, leewayPortal, "agent:inst123", 3600, 900))
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_jwt_clock_skew_leeway.py TestParseLeeway test_empty_uses_default
func TestParseLeewayEmptyUsesDefault(t *testing.T) {
	if parseLeeway(nil) != DefaultTokenLeewaySeconds || parseLeeway("") != DefaultTokenLeewaySeconds {
		t.Fatal(parseLeeway(nil), parseLeeway(""))
	}
}

// test_jwt_clock_skew_leeway.py TestParseLeeway test_invalid_fails_closed_to_zero
func TestParseLeewayInvalidFailsClosedToZero(t *testing.T) {
	for _, bad := range []any{"not-a-number", "-5", "nan", "inf", "-inf", math.NaN(), math.Inf(1)} {
		if got := parseLeeway(bad); got != 0 {
			t.Fatalf("%v: %v", bad, got)
		}
	}
}

// test_jwt_clock_skew_leeway.py TestParseLeeway test_valid_values
func TestParseLeewayValidValues(t *testing.T) {
	if parseLeeway("0") != 0 || parseLeeway(30) != 30 || parseLeeway("120.5") != 120.5 {
		t.Fatal(parseLeeway("0"), parseLeeway(30), parseLeeway("120.5"))
	}
}

// test_jwt_clock_skew_leeway.py TestParseLeeway test_explicit_zero_is_respected
func TestParseLeewayExplicitZeroIsRespected(t *testing.T) {
	if parseLeeway(0) != 0 {
		t.Fatal(parseLeeway(0))
	}
}

// test_jwt_clock_skew_leeway.py TestParseLeeway test_finite_check
func TestParseLeewayFiniteCheck(t *testing.T) {
	if math.IsInf(parseLeeway("60"), 0) {
		t.Fatal("infinite")
	}
}

func selfHostedLeewayFromConfig(t *testing.T, leewayLine string) float64 {
	t.Helper()
	oidcRegisterCase(t, "    self_hosted:\n      issuer: "+oidcIssuer+"\n      client_id: "+oidcClientID+"\n"+leewayLine)
	p := registerSelfHosted(t)
	if p == nil {
		t.Fatal("not registered")
	}
	return p.idTokenLeeway
}

// test_jwt_clock_skew_leeway.py TestSelfHostedConfig test_leeway_from_config_yaml
func TestSelfHostedConfigLeewayFromConfigYAML(t *testing.T) {
	if got := selfHostedLeewayFromConfig(t, "      id_token_leeway: 300\n"); got != 300 {
		t.Fatal(got)
	}
}

// test_jwt_clock_skew_leeway.py TestSelfHostedConfig test_unset_leeway_uses_default
func TestSelfHostedConfigUnsetLeewayUsesDefault(t *testing.T) {
	if got := selfHostedLeewayFromConfig(t, ""); got != DefaultTokenLeewaySeconds {
		t.Fatal(got)
	}
}

// test_jwt_clock_skew_leeway.py TestSelfHostedConfig test_invalid_config_leeway_fails_closed
func TestSelfHostedConfigInvalidConfigLeewayFailsClosed(t *testing.T) {
	if got := selfHostedLeewayFromConfig(t, "      id_token_leeway: garbage\n"); got != 0 {
		t.Fatal(got)
	}
}

// test_jwt_clock_skew_leeway.py TestSelfHostedConfig test_explicit_leeway_bounds_tolerance
func TestSelfHostedConfigExplicitLeewayBoundsTolerance(t *testing.T) {
	s, err := leewaySelfHosted(t, 0).VerifySession(context.Background(), mintSkewedToken(t, oidcIssuer, oidcClientID, 60, 900))
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

func nousLeewayFromConfig(t *testing.T, leewayLine string) float64 {
	t.Helper()
	nousRegisterCase(t, "    client_id: agent:inst123\n"+leewayLine)
	p := registerNous(t)
	if p == nil {
		t.Fatal("not registered")
	}
	return p.tokenLeeway
}

// test_jwt_clock_skew_leeway.py TestNousConfig test_leeway_from_config_yaml
func TestNousConfigLeewayFromConfigYAML(t *testing.T) {
	if got := nousLeewayFromConfig(t, "    token_leeway: 120\n"); got != 120 {
		t.Fatal(got)
	}
}

// test_jwt_clock_skew_leeway.py TestNousConfig test_unset_leeway_uses_default
func TestNousConfigUnsetLeewayUsesDefault(t *testing.T) {
	if got := nousLeewayFromConfig(t, ""); got != DefaultTokenLeewaySeconds {
		t.Fatal(got)
	}
}

// test_jwt_clock_skew_leeway.py TestNousConfig test_invalid_config_leeway_fails_closed
func TestNousConfigInvalidConfigLeewayFailsClosed(t *testing.T) {
	if got := nousLeewayFromConfig(t, "    token_leeway: -3\n"); got != 0 {
		t.Fatal(got)
	}
}
