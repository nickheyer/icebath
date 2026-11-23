package config

import (
	"errors"
	"strings"
	"testing"
)

type fakeCatalog struct {
	discovered, reset bool
	known             map[string]string
}

func (c *fakeCatalog) DiscoverPlugins() error {
	c.discovered = true
	return errors.New("broken plugin tree")
}
func (c *fakeCatalog) ResetLanguageCache() { c.reset = c.discovered }
func (c *fakeCatalog) ResolveLanguageID(v string) (string, bool) {
	if !c.reset {
		return "", false
	}
	id, ok := c.known[strings.ToLower(v)]
	return id, ok
}
func (c *fakeCatalog) SupportedLanguages() []string { return []string{"en", "pl"} }

func TestDisplayLanguageErrorAfterDiscovery(t *testing.T) {
	cat := &fakeCatalog{known: map[string]string{"pl": "pl", "pl-pl": "pl"}}
	if got := DisplayLanguageError("PL-pl", cat); got != "" || !cat.reset {
		t.Fatalf("installed pack refused: %q", got)
	}
	if got := DisplayLanguageError("  ", &fakeCatalog{}); got != "" {
		t.Fatalf("blank refused: %q", got)
	}
	got := DisplayLanguageError("xx", &fakeCatalog{})
	if !strings.HasPrefix(got, "✗ Unknown language 'xx' for display.language. Available: en, pl.\n  Install a language pack plugin") {
		t.Fatalf("got %q", got)
	}
}
