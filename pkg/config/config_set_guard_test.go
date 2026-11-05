package config

import (
	"errors"
	"strings"
	"testing"
)

func TestUnknownSubkeyRefusalWording(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	got := UnknownSubkeyRefusal("gateway.discord.foo", "discord.foo")
	want := "✗ 'gateway.discord.foo' is not a recognized config key — nothing was written.\n  Did you mean: discord.foo\n" +
		"  (Custom top-level keys are supported; use --force to write this path anyway.)"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(UnknownSubkeyRefusal("x.y", ""), "Did you mean") {
		t.Fatal("suggestion line without a suggestion")
	}
}

func TestGuardBareModelRedirectsAndForceReplaces(t *testing.T) {
	section := map[string]any{"model": map[string]any{"provider": "openai-api", "default": "x", "context_length": 128000}}
	var out strings.Builder
	key, err := GuardSectionOverwrite("model", "claude-sonnet-4-20250514", section, false, &out)
	if err != nil || key != "model.default" || !strings.Contains(out.String(), "preserving 3 existing model sub-key(s)") {
		t.Fatalf("redirect = %q %v %q", key, err, out.String())
	}
	out.Reset()
	key, err = GuardSectionOverwrite("model", "claude-opus-4", section, true, &out)
	if err != nil || key != "model" || !strings.Contains(out.String(), "with a scalar (discarding 3") {
		t.Fatalf("force scalar = %q %v %q", key, err, out.String())
	}
	out.Reset()
	key, err = GuardSectionOverwrite("model", map[string]any{"provider": "p", "default": "d"}, section, true, &out)
	if err != nil || key != "model" || !strings.Contains(out.String(), "with the given mapping") {
		t.Fatalf("force mapping = %q %v %q", key, err, out.String())
	}
	if key, err := GuardSectionOverwrite("model", "gpt-5.6-sol", map[string]any{}, false, &out); key != "model" || err != nil {
		t.Fatalf("no section = %q %v", key, err)
	}
	if key, err := GuardSectionOverwrite("model.default", "claude-opus-4", section, false, &out); key != "model.default" || err != nil {
		t.Fatalf("dotted = %q %v", key, err)
	}
}

func TestGuardBareModelContainerLiteralRefused(t *testing.T) {
	for _, c := range []struct {
		start map[string]any
		value any
	}{
		{map[string]any{}, []any{"a", "b"}},
		{map[string]any{"model": "gpt"}, []any{"a"}},
		{map[string]any{"model": map[string]any{"provider": "p"}}, map[string]any{"default": "d"}},
	} {
		var out strings.Builder
		_, err := GuardSectionOverwrite("model", c.value, c.start, false, &out)
		var refusal *ConfigSetRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "Cannot set 'model' to a") {
			t.Fatalf("%v accepted: %v", c.value, err)
		}
	}
}

func TestGuardNonModelSectionRefusedWithoutForce(t *testing.T) {
	section := map[string]any{"terminal": map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8, "i": 9, "j": 10}}
	var out strings.Builder
	_, err := GuardSectionOverwrite("terminal", "zsh", section, false, &out)
	var refusal *ConfigSetRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("not refused: %v", err)
	}
	for _, want := range []string{"is a configuration section with 10 sub-key(s)", "Sub-keys: a, b, c, d, e, f, g, h", "... and 2 more",
		"icebath config set terminal.<sub-key> <value>", "icebath config set --force terminal 'zsh'"} {
		if !strings.Contains(refusal.Message, want) {
			t.Fatalf("missing %q in %q", want, refusal.Message)
		}
	}
	if key, err := GuardSectionOverwrite("terminal", "zsh", section, true, &out); key != "terminal" || err != nil {
		t.Fatalf("force = %q %v", key, err)
	}
}

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
