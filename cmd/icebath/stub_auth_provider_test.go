package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Key the stub signs its opaque tokens with
var stubSecret = []byte("stub-test-secret-not-for-prod")

// Opaque token of the payload, the url safe base64 of its compact JSON and HMAC suffix
func stubSign(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, stubSecret)
	mac.Write(raw)
	return base64.URLEncoding.EncodeToString(append(raw, mac.Sum(nil)...))
}

// Payload of a token the stub signed, nil for any tamper or decode failure
func stubUnsign(token string) map[string]any {
	blob, err := base64.URLEncoding.DecodeString(token)
	if err != nil || len(blob) <= sha256.Size {
		return nil
	}
	raw, sig := blob[:len(blob)-sha256.Size], blob[len(blob)-sha256.Size:]
	mac := hmac.New(sha256.New, stubSecret)
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	return payload
}

// Random url safe text of n bytes, the secrets.token_urlsafe of the stub
func stubTokenURLSafe(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Local fake identity provider whose tokens are HMAC signed JSON, the StubAuthProvider of the hermes auth cases
type stubAuthProvider struct {
	ttl             int64
	mu              sync.Mutex
	stateToVerifier map[string]string
}

func newStubAuthProvider() *stubAuthProvider {
	return &stubAuthProvider{ttl: 3600, stateToVerifier: map[string]string{}}
}

func (p *stubAuthProvider) Name() string { return "stub" }

func (p *stubAuthProvider) DisplayName() string { return "Stub IdP (test only)" }

func (p *stubAuthProvider) StartLogin(_ context.Context, redirectURI string) (gateway.LoginStart, error) {
	state, verifier := stubTokenURLSafe(16), stubTokenURLSafe(32)
	p.mu.Lock()
	p.stateToVerifier[state] = verifier
	p.mu.Unlock()
	return gateway.LoginStart{RedirectURL: fmt.Sprintf("%s?code=stub_code&state=%s", redirectURI, state),
		CookiePayload: map[string]string{"hermes_session_pkce": fmt.Sprintf("state=%s;verifier=%s", state, verifier)}}, nil
}

func (p *stubAuthProvider) CompleteLogin(_ context.Context, code, state, codeVerifier, _ string) (*gateway.DashboardSession, error) {
	if code != "stub_code" {
		return nil, &gateway.InvalidCodeError{Msg: fmt.Sprintf("stub expects code='stub_code', got '%s'", code)}
	}
	p.mu.Lock()
	expected, known := p.stateToVerifier[state]
	if known && expected == codeVerifier {
		delete(p.stateToVerifier, state)
	}
	p.mu.Unlock()
	if !known || expected != codeVerifier {
		return nil, &gateway.InvalidCodeError{Msg: "stub state/verifier mismatch"}
	}
	return p.issue("stub-user-1"), nil
}

// Session of the subject with a fresh access token and a thirty day refresh token
func (p *stubAuthProvider) issue(subject string) *gateway.DashboardSession {
	now := time.Now().Unix()
	exp := now + p.ttl
	return &gateway.DashboardSession{UserID: subject, Email: "stub@example.test", DisplayName: "Stub User", OrgID: "stub-org-1",
		Provider: p.Name(), ExpiresAt: exp,
		AccessToken: stubSign(map[string]any{"sub": subject, "email": "stub@example.test", "name": "Stub User",
			"org_id": "stub-org-1", "exp": exp}),
		RefreshToken: stubSign(map[string]any{"sub": subject, "kind": "refresh", "exp": now + 30*86400})}
}

// Expiry of a payload, zero when absent
func stubExpiry(payload map[string]any) int64 {
	exp, _ := payload["exp"].(float64)
	return int64(exp)
}

func (p *stubAuthProvider) VerifySession(_ context.Context, accessToken string) (*gateway.DashboardSession, error) {
	payload := stubUnsign(accessToken)
	if payload == nil || stubExpiry(payload) <= time.Now().Unix() {
		return nil, nil
	}
	text := func(key string) string { value, _ := payload[key].(string); return value }
	return &gateway.DashboardSession{UserID: text("sub"), Email: text("email"), DisplayName: text("name"), OrgID: text("org_id"),
		Provider: p.Name(), ExpiresAt: stubExpiry(payload), AccessToken: accessToken}, nil
}

func (p *stubAuthProvider) RefreshSession(_ context.Context, refreshToken string) (*gateway.DashboardSession, error) {
	payload := stubUnsign(refreshToken)
	if payload == nil || stubExpiry(payload) <= time.Now().Unix() {
		return nil, &gateway.RefreshExpiredError{Msg: "stub refresh token expired/invalid"}
	}
	subject, _ := payload["sub"].(string)
	return p.issue(subject), nil
}

func (p *stubAuthProvider) RevokeSession(context.Context, string) error { return nil }
