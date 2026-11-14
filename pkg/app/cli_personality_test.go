package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

// CLI holding the user personalities under agent.personalities with a kawaii system prompt
func personalityCLI(t *testing.T, user map[string]any) *CLI {
	t.Helper()
	recordPrints(t)
	if user == nil {
		user = map[string]any{"helpful": "You are helpful.", "concise": "You are concise."}
	}
	c := bareCLI()
	c.config = map[string]any{"agent": map[string]any{"personalities": user}}
	c.personalities = commands.AvailablePersonalities(config.Tree(c.config))
	c.systemPrompt = "You are kawaii~"
	return c
}

// Raw config.yaml of the test home
func homeConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Value at the dotted path of a raw config, nil when absent
func dig(cfg map[string]any, path string) any {
	var cur any = cfg
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

func TestCLIPersonalityNoneSetPersistsDisplayPersonalityNotSystemPrompt(t *testing.T) {
	c := personalityCLI(t, nil)
	c.handlePersonalityCommand("/personality helpful")
	cfg := homeConfig(t)
	if c.systemPrompt != "You are helpful." || dig(cfg, "display.personality") != "helpful" || dig(cfg, "agent.system_prompt") != nil {
		t.Fatalf("prompt %q config %v", c.systemPrompt, cfg)
	}
}

func TestCLIPersonalityNoneNeutralRestoresManualSystemPromptWithoutWipingConfig(t *testing.T) {
	c := personalityCLI(t, nil)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("agent:\n  system_prompt: manual forever\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.handlePersonalityCommand("/personality neutral")
	cfg := homeConfig(t)
	if c.systemPrompt != "manual forever" || dig(cfg, "display.personality") != "" || dig(cfg, "agent.system_prompt") != "manual forever" {
		t.Fatalf("prompt %q config %v", c.systemPrompt, cfg)
	}
}

func TestCLIPersonalityNoneBuiltinPersonalityWorksWithoutConfigEntry(t *testing.T) {
	c := personalityCLI(t, map[string]any{})
	c.handlePersonalityCommand("/personality kawaii")
	if !strings.Contains(strings.ToLower(c.systemPrompt), "kawaii") {
		t.Fatalf("prompt %q", c.systemPrompt)
	}
}

func TestPersonalityDictFormatDictPersonalityUsesSystemPrompt(t *testing.T) {
	c := personalityCLI(t, map[string]any{"coder": map[string]any{"description": "Expert programmer",
		"system_prompt": "You are an expert programmer.", "tone": "technical", "style": "concise"}})
	c.systemPrompt = ""
	c.handlePersonalityCommand("/personality coder")
	if !strings.Contains(c.systemPrompt, "You are an expert programmer.") {
		t.Fatalf("prompt %q", c.systemPrompt)
	}
}

func TestPersonalityListingKeepsUserEntriesInConfigFileOrder(t *testing.T) {
	recordPrints(t)
	h := os.Getenv("ICEBATH_HOME")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  personalities:\n    zeta: You are zeta.\n    alpha: You are alpha.\n    mid: You are mid.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadContext(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	c.config = cfg
	c.personalities = commands.AvailablePersonalities(config.Tree(c.config))
	names := c.personalityNames()
	if got := names[len(names)-3:]; !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("user entries %q", got)
	}
}
