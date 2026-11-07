package tools

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Exported xAI user agent matches the unexported one and carries the icebath product token
func TestIcebathXAIUserAgentExport(t *testing.T) {
	if got := IcebathXAIUserAgent(); got != icebathXAIUserAgent() || !strings.HasPrefix(got, "Icebath-Agent/") {
		t.Fatalf("IcebathXAIUserAgent() = %q", got)
	}
}

func TestResolveXAIHTTPCredentialsExportSplitsTheCredentials(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	c := resolveXAIHTTPCredentials(env, true)
	if p, k, u := ResolveXAIHTTPCredentials(env, true); p != c.Provider || k != c.APIKey || u != c.BaseURL {
		t.Fatalf("xai credentials %q %q %q", p, k, u)
	}
}
