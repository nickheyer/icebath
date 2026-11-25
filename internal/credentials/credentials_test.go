package credentials

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type fixed map[string]Credential

func (f fixed) Credential(name string) (Credential, bool) {
	credential, ok := f[name]
	return credential, ok
}

func TestRegistryResolvesRoutes(t *testing.T) {
	host := New()
	nested := new(Registry)
	host.Register(nested)
	nested.Register(fixed{"keyed": {APIKey: "first", Source: "one"}})
	host.Register(fixed{
		"keyed":  {APIKey: "second", Source: "two"},
		"moved":  {APIKey: "k", BaseURL: "https://elsewhere.test/v1"},
		"bearer": {APIKey: "token", Auth: provider.AuthOAuth},
	})

	if credential, ok := host.Credential("keyed"); !ok || credential.APIKey != "first" {
		t.Fatalf("Credential(\"keyed\") = %+v, %v, want the first registrant that answers", credential, ok)
	}
	if _, ok := host.Credential("none"); ok {
		t.Fatal("Credential(\"none\") resolved, want not found")
	}

	headers := map[string]string{"X-Title": "t"}
	route, err := host.Route(provider.Profile{Name: "keyed", BaseURL: "https://api.test/v1", Mode: provider.ModeChatCompletions, Auth: provider.AuthAPIKey, DefaultModel: "default", Headers: headers}, "")
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	want := provider.Route{Provider: "keyed", Model: "default", BaseURL: "https://api.test/v1", APIKey: "first", Headers: headers, Mode: provider.ModeChatCompletions, Auth: provider.AuthAPIKey}
	if route.Provider != want.Provider || route.Model != want.Model || route.BaseURL != want.BaseURL || route.APIKey != want.APIKey || route.Mode != want.Mode || route.Auth != want.Auth || route.Headers["X-Title"] != "t" {
		t.Fatalf("Route() = %+v, want %+v", route, want)
	}
	if route, _ := host.Route(provider.Profile{Name: "keyed", BaseURL: "https://api.test/v1", DefaultModel: "default"}, "chosen"); route.Model != "chosen" {
		t.Fatalf("Route() model = %q, want the requested model over the default", route.Model)
	}
	if route, _ := host.Route(provider.Profile{Name: "moved", BaseURL: "https://api.test/v1", Auth: provider.AuthAPIKey}, "m"); route.BaseURL != "https://elsewhere.test/v1" {
		t.Fatalf("Route() base url = %q, want the one the credential carries", route.BaseURL)
	}
	if route, _ := host.Route(provider.Profile{Name: "bearer", BaseURL: "https://api.test", Auth: provider.AuthAPIKey}, "m"); route.Auth != provider.AuthOAuth {
		t.Fatalf("Route() auth = %q, want the kind the credential carries", route.Auth)
	}
	if _, err := host.Route(provider.Profile{Name: "none", BaseURL: "https://api.test", Auth: provider.AuthAPIKey}, "m"); err == nil || !strings.Contains(err.Error(), "provider none: no credential") {
		t.Fatalf("Route() error = %v, want a missing credential error", err)
	}
	if route, err := host.Route(provider.Profile{Name: "none", BaseURL: "http://localhost:1/v1", Auth: provider.AuthAPIKey, Local: true}, "m"); err != nil || route.APIKey != "" {
		t.Fatalf("Route() of a local profile = %+v, %v, want a keyless route", route, err)
	}
	if _, err := host.Route(provider.Profile{Name: "none", BaseURL: "http://localhost:1/v1", Auth: provider.AuthNone}, "m"); err != nil {
		t.Fatalf("Route() of a profile without auth error = %v, want none", err)
	}
	if _, err := host.Route(provider.Profile{Name: "keyed", Auth: provider.AuthAPIKey}, "m"); err == nil || !strings.Contains(err.Error(), "no base url") {
		t.Fatalf("Route() error = %v, want a missing base url error", err)
	}
}

func TestLookupNamesTheSource(t *testing.T) {
	cfg := config.New(t.TempDir())
	cfg.MustBindEnv("plugins.test.api_key", "ICEBATH_TEST_FIRST_KEY", "ICEBATH_TEST_SECOND_KEY")
	if _, ok := Lookup(cfg, "plugins.test.api_key", "ICEBATH_TEST_FIRST_KEY", "ICEBATH_TEST_SECOND_KEY"); ok {
		t.Fatal("Lookup() found a credential with nothing set")
	}
	t.Setenv("ICEBATH_TEST_SECOND_KEY", "from-second")
	if credential, ok := Lookup(cfg, "plugins.test.api_key", "ICEBATH_TEST_FIRST_KEY", "ICEBATH_TEST_SECOND_KEY"); !ok || credential.APIKey != "from-second" || credential.Source != "ICEBATH_TEST_SECOND_KEY" {
		t.Fatalf("Lookup() = %+v, %v, want the second variable", credential, ok)
	}
	t.Setenv("ICEBATH_TEST_FIRST_KEY", "from-first")
	if credential, _ := Lookup(cfg, "plugins.test.api_key", "ICEBATH_TEST_FIRST_KEY", "ICEBATH_TEST_SECOND_KEY"); credential.APIKey != "from-first" || credential.Source != "ICEBATH_TEST_FIRST_KEY" {
		t.Fatalf("Lookup() = %+v, want the first variable to win", credential)
	}
	stored := config.New(t.TempDir())
	if err := stored.Set("plugins.test.api_key", "from-file"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if credential, ok := Lookup(stored, "plugins.test.api_key", "ICEBATH_TEST_UNSET_KEY"); !ok || credential.APIKey != "from-file" || credential.Source != "config" {
		t.Fatalf("Lookup() = %+v, %v, want the stored value with source config", credential, ok)
	}
}
