package scenario

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Configuration values are read, written to config.yaml or .env by kind, removed, and migrated to the current version
func TestScenarioConfigGetSetUnsetMigrate(t *testing.T) {
	s := Start(t, Setup{})
	if r := s.Run("config", "path").Exit(0); r.Stdout != s.Home+"/config.yaml\n" {
		s.Failf("config path printed %q", r.Stdout)
	}
	if r := s.Run("config", "env-path").Exit(0); r.Stdout != s.Home+"/.env\n" {
		s.Failf("config env-path printed %q", r.Stdout)
	}
	if r := s.Run("config", "get", "model.default").Exit(0); r.Stdout != "fake-model\n" {
		s.Failf("config get model.default printed %q", r.Stdout)
	}
	s.HasJSON(s.Run("config", "get", "model", "--json").Exit(0).Stdout,
		`{"provider": "custom", "base_url": "${MODEL_URL}", "default": "fake-model", "context_length": 128000}`)
	missing := s.Run("config", "get", "no.such.key").Exit(1)
	if missing.Stdout != "" || !strings.Contains(missing.Stderr, "Config key not set: no.such.key") {
		s.Failf("a key that is not set is reported on standard error only\n%s", missing.dump())
	}

	s.Run("config", "set", "agent.max_turns", "42").Exit(0).Has("✓ Set agent.max_turns = 42 in " + s.Home + "/config.yaml")
	s.HasJSON(s.YAML("config.yaml"), `{"agent": {"max_turns": 42, "api_max_retries": 1}, "model": {"default": "fake-model"}}`)
	if r := s.Run("config", "get", "agent.max_turns").Exit(0); r.Stdout != "42\n" {
		s.Failf("config get agent.max_turns printed %q", r.Stdout)
	}
	s.Run("config", "set", "display.skin", "contrast").Exit(0)
	s.HasJSON(s.YAML("config.yaml"), `{"display": {"skin": "contrast"}}`)

	s.Run("config", "set", "OPENAI_API_KEY", "sk-scenario-secret-0123456789").Exit(0).Has("✓ Set OPENAI_API_KEY in " + s.Home + "/.env")
	if env := s.Read(".env"); !strings.Contains(env, "OPENAI_API_KEY=sk-scenario-secret-0123456789") {
		s.Failf(".env does not hold the new key\n%s", env)
	}
	if strings.Contains(s.Read("config.yaml"), "sk-scenario-secret") {
		s.Failf("the API key was written into config.yaml")
	}
	s.Run("config", "get", "OPENAI_API_KEY").Exit(0).Lacks("sk-scenario-secret-0123456789")
	if r := s.Run("config", "get", "OPENAI_API_KEY", "--raw").Exit(0); r.Stdout != "sk-scenario-secret-0123456789\n" {
		s.Failf("config get --raw printed %q", r.Stdout)
	}

	s.Run("config", "unset", "agent.max_turns").Exit(0).Has("✓ Unset agent.max_turns from " + s.Home + "/config.yaml")
	if _, kept := Dig(s.YAML("config.yaml"), "agent").(map[string]any)["max_turns"]; kept {
		s.Failf("agent.max_turns survived the unset")
	}
	s.Run("config", "unset", "agent.max_turns").Exit(1).Has("Config key not set: agent.max_turns")

	old := "_config_version: 12\nmodel:\n  provider: custom\n  base_url: ${MODEL_URL}\n  default: fake-model\n"
	s.Write("config.yaml", old)
	s.Run("config", "check").Exit(0)
	migrated := s.RunStep(Step{Args: []string{"config", "migrate"}, Stdin: strings.Repeat("n\n", 12)}).Exit(0)
	migrated.Has("Checking configuration for updates", fmt.Sprintf("Config version: 12 → %d", config.Version))
	s.HasJSON(s.YAML("config.yaml"), fmt.Sprintf(`{"_config_version": %d, "model": {"provider": "custom", "default": "fake-model"}}`, config.Version))
	s.RunStep(Step{Args: []string{"config", "migrate"}, Stdin: strings.Repeat("n\n", 12)}).Exit(0).Lacks("Config version: 12 →")

	ancient := strings.Replace(s.Expand(old), "_config_version: 12", "_config_version: 1", 1)
	s.Write("config.yaml", ancient)
	refused := s.RunStep(Step{Args: []string{"config", "migrate"}, Stdin: strings.Repeat("n\n", 12)}).Exit(0)
	refused.Has("This config predates version 12 (~2 years old) and can no longer be auto-migrated.",
		"/config.yaml, then start `icebath` and type `/setup` to regenerate it, or manually set _config_version: 12 after reviewing the changelog.")
	if !strings.Contains(refused.Stderr, "⚠ icebath config: This config predates version 12") {
		s.Failf("the support floor refusal is also written to standard error\n%s", refused.dump())
	}
	if now := s.Read("config.yaml"); now != ancient {
		s.Failf("a config below the support floor was rewritten\n%s", now)
	}
}
