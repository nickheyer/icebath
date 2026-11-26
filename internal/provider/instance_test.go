package provider_test

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

type specs struct{}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{Name: "acme", DisplayName: "Acme", BaseURL: "https://acme.test/v1", DefaultModel: "a1", Mode: provider.ModeChatCompletions,
		Auth: provider.AuthAPIKey, Aliases: []string{"acme-ai"}}}
}

func setup(t *testing.T) (*config.Config, *provider.Provider, *credentials.Credentials) {
	t.Helper()
	cfg := config.New(t.TempDir())
	p := provider.New(cfg)
	p.Register(specs{})
	creds := credentials.New()
	creds.Register(credentials.Keyed{Provider: "acme", Variables: []string{"ICEBATH_TEST_ACME_KEY"}})
	if err := cfg.Use("default"); err != nil {
		t.Fatal(err)
	}
	return cfg, p, creds
}

func TestInstancesOfOneKind(t *testing.T) {
	cfg, p, _ := setup(t)
	first, err := p.Add(cfg, "acme-ai", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Add(cfg, "acme", "", "Acme two")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "acme" || second.ID != "acme-2" || second.Label() != "Acme two" {
		t.Fatalf("added %q and %q (%q)", first.ID, second.ID, second.Label())
	}
	if err := cfg.Set("providers.acme-2.base_url", "https://other.test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Instance(cfg, "acme-2"); got.Spec.BaseURL != "https://other.test" {
		t.Fatalf("base URL of the second is %q", got.Spec.BaseURL)
	}
	if got, _ := p.Instance(cfg, "acme"); got.Spec.BaseURL != "https://acme.test/v1" || got.Spec.DefaultModel != "a1" {
		t.Fatalf("the first lost its defaults: %+v", got.Spec)
	}
	if got, ok := p.Instance(cfg, "acme-ai"); !ok || got.ID != "acme" {
		t.Fatal("an alias doesn't find the first of its kind")
	}
	if _, ok := cfg.Section("providers.acme-2"); !ok {
		t.Fatal("no settings section for the second")
	}
	if err := p.Remove(cfg, "acme-2"); err != nil {
		t.Fatal(err)
	}
	if len(p.Instances(cfg)) != 1 {
		t.Fatal("removed provider is still there")
	}
}

func TestAccounts(t *testing.T) {
	cfg, p, creds := setup(t)
	if _, err := p.Add(cfg, "acme", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TEST_ACME_KEY", "from-env")
	instance, _ := p.Instance(cfg, "acme")
	if route, err := creds.Route(instance, ""); err != nil || route.APIKey != "from-env" || route.Model != "a1" {
		t.Fatalf("with no account: %+v %v", route, err)
	}
	work, err := p.AddAccount(cfg, "acme", "work", "Work")
	if err != nil {
		t.Fatal(err)
	}
	home, err := p.AddAccount(cfg, "acme", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if work != "work" || home != "default" {
		t.Fatalf("accounts named %q and %q", work, home)
	}
	for name, key := range map[string]string{"work": "work-key", "default": "home-key"} {
		if err := cfg.Set("providers.acme.accounts."+name+"."+providerconfig.APIKeyField, key); err != nil {
			t.Fatal(err)
		}
	}
	instance, _ = p.Instance(cfg, "acme")
	if route, _ := creds.Route(instance, ""); route.APIKey != "work-key" {
		t.Fatalf("first account added isn't in use: %q", route.APIKey)
	}
	if err := p.UseAccount(cfg, "acme", "default"); err != nil {
		t.Fatal(err)
	}
	instance, _ = p.Instance(cfg, "acme")
	if route, _ := creds.Route(instance, ""); route.APIKey != "home-key" {
		t.Fatalf("switched account gives %q", route.APIKey)
	}
	if err := p.RemoveAccount(cfg, "acme", "default"); err != nil {
		t.Fatal(err)
	}
	instance, _ = p.Instance(cfg, "acme")
	if instance.Account != "work" {
		t.Fatalf("after removing the one in use, %q is", instance.Account)
	}
}
