package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func refreshOwner(a *AIAgent) string {
	if p := credentialRefreshProfile(context.Background(), a); p != nil {
		return p.Name
	}
	return ""
}

func TestRuntimeRefreshOwnerIsFoundByNameOrByHostOfACustomRoute(t *testing.T) {
	caseHome(t)
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "seam-acme", BaseURL: "https://api.seam-acme.example/v1", Hostnames: []string{"seam-acme.example"},
		RefreshRuntimeCredentialsHook: func(_ context.Context, reason string) (string, string, bool, error) {
			return "fresh", "", reason == "unauthorized", nil
		}})
	provider.RegisterProvider(provider.Profile{Name: "seam-plain", BaseURL: "https://api.seam-plain.example/v1"})
	if got := refreshOwner(&AIAgent{provider: "seam-acme"}); got != "seam-acme" {
		t.Fatalf("by name %q", got)
	}
	if got := refreshOwner(&AIAgent{provider: "custom", baseURL: "https://api.seam-acme.example/v1"}); got != "seam-acme" {
		t.Fatalf("by host of a custom route %q", got)
	}
	if got := refreshOwner(&AIAgent{provider: "seam-plain", baseURL: "https://api.seam-plain.example"}); got != "" {
		t.Fatalf("a profile that mints nothing owned a refresh %q", got)
	}
}

func TestRefreshProviderCheckAcceptsAliasSpellings(t *testing.T) {
	caseHome(t)
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "acme-refresh", Aliases: []string{"acme-cloud-refresh", "acme"},
		BaseURL: "https://api.acme-refresh.test", Hostnames: []string{"acme-refresh.test"},
		RefreshRuntimeCredentialsHook: func(context.Context, string) (string, string, bool, error) { return "", "", false, nil }})
	owner := func(name, baseURL string) string { return refreshOwner(&AIAgent{provider: name, baseURL: baseURL}) }
	for _, spelling := range []string{"acme-refresh", "acme-cloud-refresh", "ACME-Cloud-Refresh", "acme"} {
		if got := owner(spelling, ""); got != "acme-refresh" {
			t.Fatalf("%s owner %q", spelling, got)
		}
	}
	if got := owner("custom", "https://api.acme-refresh.test"); got != "acme-refresh" {
		t.Fatalf("url owner %q", got)
	}
	if got := owner("openrouter", "https://openrouter.ai/api/v1"); got != "" {
		t.Fatalf("openrouter owner %q", got)
	}
}
