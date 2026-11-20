package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestOAuthLoginWindowFollowsOAuthTimeout(t *testing.T) {
	cfg := map[string]any{"url": "https://mcp.example.test/mcp", "auth": "oauth", "oauth": map[string]any{"timeout": 3600, "redirect_port": 27891}}
	if got := LoginConnectTimeout(cfg); got != 3615.0 {
		t.Fatalf("connect timeout %v", got)
	}
}

func TestOAuthTokenWriteAfterProfileDeleteDoesNotRecreateHome(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profiles", "p1")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := home.MarkProfileDeleted(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(profile); err != nil {
		t.Fatal(err)
	}
	if home.NamedProfileHome(profile) == "" || exists(profile) {
		t.Fatal("profile not tombstoned")
	}
	err := writeOAuthJSON(context.Background(), filepath.Join(profile, "mcp-oauth", "tokens.json"), map[string]any{"access_token": "x"})
	var missing *home.ErrProfileMissing
	if !errors.As(err, &missing) || !strings.Contains(err.Error(), "Named profile home does not exist") {
		t.Fatalf("err %v", err)
	}
	if exists(profile) {
		t.Fatal("deleted profile home recreated")
	}
}

func TestOAuthFigmaRemoteMCPHostAnchored(t *testing.T) {
	for _, c := range []struct {
		name, url string
		want      bool
	}{
		{"", "https://mcp.figma.com/mcp", true},
		{"", "https://www.figma.com/mcp", true},
		{"", "https://evil.example/mcp.figma.com/mcp", false},
		{"", "https://figma.com.evil.io/mcp", false},
		{"figma", "https://phish.example/figma", false},
		{"figma", "", true},
	} {
		if got := isFigmaRemoteMCP(c.name, c.url); got != c.want {
			t.Fatalf("%q %q gave %v", c.name, c.url, got)
		}
	}
}
