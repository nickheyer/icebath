package mcp

import (
	"context"
	"path/filepath"
	"testing"
)

// Token file stamped with issuer (none when empty) loaded through a fresh storage
func storedIssuerTokens(t *testing.T, dir, issuer string) (*TokenStorage, *OAuthToken) {
	t.Helper()
	payload := map[string]any{"access_token": "a", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "r"}
	if issuer != "" {
		payload["hermes_issuer"] = issuer
	}
	writeTokenFile(t, dir, "srv.json", payload)
	s := NewTokenStorage(context.Background(), "srv", dir)
	tok, err := s.GetTokens()
	if err != nil || tok == nil {
		t.Fatalf("tokens %v %v", tok, err)
	}
	return s, tok
}

// Provider whose discovered issuer and current tokens are set without a flow
func issuerProvider(s *TokenStorage, issuer string, tok *OAuthToken) *OAuthProvider {
	p := &OAuthProvider{storage: s, currentTokens: tok}
	if issuer != "" {
		p.oauthMetadata = &OAuthMetadata{Issuer: issuer}
	}
	return p
}

func TestOAuthIssuerMismatchStripsRefreshTokenInMemoryAndOnDisk(t *testing.T) {
	dir := t.TempDir()
	s, tok := storedIssuerTokens(t, dir, "https://old.example.com")
	p := issuerProvider(s, "https://evil.example.com", tok)
	p.enforceRefreshTokenIssuer()
	if p.currentTokens.RefreshToken != nil || p.currentTokens.AccessToken != "a" {
		t.Fatalf("tokens %+v", p.currentTokens)
	}
	onDisk := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))
	if _, ok := onDisk["refresh_token"]; ok || onDisk["access_token"] != "a" {
		t.Fatalf("on disk %v", onDisk)
	}
}

func TestOAuthIssuerMatchingKeepsRefreshTokenEvenWithTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	s, tok := storedIssuerTokens(t, dir, "https://as.example.com/")
	p := issuerProvider(s, "https://as.example.com", tok)
	p.enforceRefreshTokenIssuer()
	if derefStr(p.currentTokens.RefreshToken) != "r" || readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))["refresh_token"] != "r" {
		t.Fatal("matching issuer lost the refresh token")
	}
}

func TestOAuthIssuerLegacyFileWithoutIssuerAdoptsCurrentIssuerOnce(t *testing.T) {
	dir := t.TempDir()
	s, tok := storedIssuerTokens(t, dir, "")
	issuerProvider(s, "https://as.example.com", tok).enforceRefreshTokenIssuer()
	if derefStr(tok.RefreshToken) != "r" || readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))["hermes_issuer"] != "https://as.example.com" {
		t.Fatal("legacy file did not adopt the current issuer")
	}
	s2 := NewTokenStorage(context.Background(), "srv", dir)
	tok2, err := s2.GetTokens()
	if err != nil || tok2 == nil {
		t.Fatal(err)
	}
	issuerProvider(s2, "https://evil.example.com", tok2).enforceRefreshTokenIssuer()
	if tok2.RefreshToken != nil {
		t.Fatal("adopted issuer did not guard the refresh token")
	}
}

func TestOAuthIssuerSetTokensStampsBoundIssuerAndGetTokensKeepsItOutOfModel(t *testing.T) {
	dir := t.TempDir()
	s := NewTokenStorage(context.Background(), "srv", dir)
	issuerProvider(s, "https://as.example.com", nil).bindIssuerFromContext()
	tok := bearer("a", "r")
	tok.ExpiresIn = int64Ptr(3600)
	if err := s.SetTokens(tok); err != nil {
		t.Fatal(err)
	}
	if got := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "srv.json"))["hermes_issuer"]; got != "https://as.example.com" {
		t.Fatalf("stamped %v", got)
	}
	fresh := NewTokenStorage(context.Background(), "srv", dir)
	loaded, err := fresh.GetTokens()
	if err != nil || loaded == nil || fresh.LoadedIssuer() != "https://as.example.com" {
		t.Fatalf("loaded %+v issuer %q %v", loaded, fresh.LoadedIssuer(), err)
	}
	if _, ok := loaded.dump().vals["hermes_issuer"]; ok {
		t.Fatal("issuer leaked into the token model")
	}
}

// Reports whether a redirect carrying iss passes the RFC 9207 check after the known server tolerance
func redirectPassesIssCheck(t *testing.T, issuer, iss string) bool {
	t.Helper()
	md, err := validateOAuthMetadata(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/oauth",
		"token_endpoint": issuer + "/token", "authorization_response_iss_parameter_supported": true}, false)
	if err != nil {
		t.Fatal(err)
	}
	p := &OAuthProvider{oauthMetadata: md, oauthFlow: "browser",
		callbackHandler: func(context.Context) (*AuthorizationCodeResult, error) {
			return &AuthorizationCodeResult{Code: "c0de", State: "st", Iss: iss}, nil
		}}
	p.tolerateMissingIssForKnownServer()
	result, err := p.callbackHandler(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return validateAuthorizationResponseIss(result.Iss, p.oauthMetadata) == nil
}

func TestOAuthMissingIssIsToleratedForFigmaOnly(t *testing.T) {
	for _, c := range []struct {
		issuer, iss string
		want        bool
	}{{"https://api.figma.com", "", true}, {"https://api.figma.com", "https://evil.example", false}, {"https://auth.example.com", "", false}} {
		if got := redirectPassesIssCheck(t, c.issuer, c.iss); got != c.want {
			t.Fatalf("%s iss %q passed=%v", c.issuer, c.iss, got)
		}
	}
}
