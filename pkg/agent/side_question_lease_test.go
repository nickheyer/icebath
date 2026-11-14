package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"gopkg.in/yaml.v3"
)

type sqCall struct {
	pluginID, prompt string
	timeout          float64
	capture          *sideQuestionCapture
}

func sqWriteYAML(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Plugin directory whose manifest declares llm.side_question or nothing
func sqInstallPlugin(t *testing.T, name string, declares bool) {
	manifest := map[string]any{"name": name}
	if declares {
		manifest["capabilities"] = []string{sideQuestionCapabilityID}
	}
	dir := filepath.Join(os.Getenv("ICEBATH_HOME"), "plugins", name)
	sqWriteYAML(t, filepath.Join(dir, "plugin.yaml"), manifest)
	body := "import json, os, pathlib\n\ndef register(ctx):\n    def _on_post_llm_call(**kwargs):\n        pass\n    ctx.register_hook('post_llm_call', _on_post_llm_call)\n"
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Writes the enabled and granted plugin config and rediscovers the active manager
func sqLoad(t *testing.T, enabled, granted []string) *plugins.Manager {
	entries := map[string]any{}
	for _, name := range granted {
		entries[name] = map[string]any{"granted_capabilities": []string{sideQuestionCapabilityID}}
	}
	sqWriteYAML(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"),
		map[string]any{"plugins": map[string]any{"enabled": enabled, "entries": entries}})
	m := plugins.GetPluginManager(context.Background())
	m.DiscoverAndLoad(context.Background(), true)
	return m
}

func sqConsented(t *testing.T) *plugins.Manager {
	sqInstallPlugin(t, "next-prompt", true)
	sqInstallPlugin(t, "bystander", false)
	return sqLoad(t, []string{"next-prompt", "bystander"}, []string{"next-prompt"})
}

func sqCtx(m *plugins.Manager, name string) *plugins.Context {
	return plugins.NewContext(&plugins.Manifest{Name: name, Key: name}, m)
}

func sqAgent(turnID string) *AIAgent {
	return &AIAgent{cachedSystemPrompt: "sys", tools: []any{}, sessionID: "sess", currentTurnID: turnID}
}

func sqIssue(t *testing.T, a *AIAgent, messages []map[string]any, turnID string) any {
	t.Helper()
	token, err := issueSideQuestionToken(context.Background(), a, messages, turnID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func sqRedeem(t *testing.T, token, prompt any, pluginID string, timeout any) *string {
	t.Helper()
	answer, err := RedeemSideQuestion(context.Background(), token, prompt, pluginID, "", timeout)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func sqAsk(t *testing.T, c *plugins.Context, token any) *string {
	t.Helper()
	answer, err := c.SideQuestion(context.Background(), token, "q", 15.0)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func sqIsAnswer(got *string, want string) bool { return got != nil && *got == want }
