package mcp

import (
	"context"
	"strings"
	"testing"
)

// Authorization server metadata with the given token endpoint
func makeTestMetadata(t *testing.T, tokenEndpoint string) *OAuthMetadata {
	t.Helper()
	md, err := validateOAuthMetadata(map[string]any{"issuer": "https://auth.example.com",
		"authorization_endpoint": "https://auth.example.com/oauth/authorize", "token_endpoint": tokenEndpoint,
		"response_types_supported": []any{"code"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestOAuthMetadataSaveAndLoadRoundtrip(t *testing.T) {
	oauthHome(t)
	s := NewTokenStorage(context.Background(), "example-server", "")
	if err := s.SaveOAuthMetadata(context.Background(), makeTestMetadata(t, "https://auth.example.com/oauth/token")); err != nil {
		t.Fatal(err)
	}
	if !exists(s.MetaPath()) {
		t.Fatal("meta file missing")
	}
	md := s.LoadOAuthMetadata()
	if md == nil || md.TokenEndpoint != "https://auth.example.com/oauth/token" || strings.TrimRight(md.Issuer, "/") != "https://auth.example.com" {
		t.Fatalf("loaded %+v", md)
	}
}

func TestOAuthMetadataRemoveDeletesMetaFile(t *testing.T) {
	oauthHome(t)
	s := NewTokenStorage(context.Background(), "cleanup-server", "")
	if err := s.SaveOAuthMetadata(context.Background(), makeTestMetadata(t, "https://auth.example.com/oauth/token")); err != nil || !exists(s.MetaPath()) {
		t.Fatalf("save %v", err)
	}
	if err := s.Remove(false); err != nil || exists(s.MetaPath()) {
		t.Fatalf("remove %v", err)
	}
}

func TestOAuthMetadataInitializeRestoresMetadataFromDisk(t *testing.T) {
	oauthHome(t)
	s := NewTokenStorage(context.Background(), "mgr-srv", "")
	if err := s.SaveOAuthMetadata(context.Background(), makeTestMetadata(t, "https://mgr.example.com/token")); err != nil {
		t.Fatal(err)
	}
	p := newTestProvider(t, s, "https://example.com", nil)
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.oauthMetadata == nil || p.oauthMetadata.TokenEndpoint != "https://mgr.example.com/token" {
		t.Fatalf("metadata %+v", p.oauthMetadata)
	}
}
