package scenario

import (
	"strings"
	"testing"
)

// Toolsets are listed with their state and one is disabled and enabled again for the CLI platform
func TestScenarioToolsListDisableEnable(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("tools", "list").Exit(0).Has("Built-in toolsets (cli):", "✓ enabled  web", "✓ enabled  terminal", "✓ enabled  file", "✓ enabled  memory",
		"✗ disabled  kanban", "Plugin toolsets (cli):")
	s.Run("tools", "disable", "web").Exit(0)
	s.Run("tools", "list").Exit(0).Has("✗ disabled  web", "✓ enabled  terminal")
	enabled, _ := Dig(s.YAML("config.yaml"), "platform_toolsets", "cli").([]any)
	for _, name := range enabled {
		if name == "web" {
			s.Failf("config.yaml platform_toolsets.cli still lists web after the disable: %v", enabled)
		}
	}
	s.Run("tools", "enable", "web").Exit(0)
	s.Run("tools", "list").Exit(0).Has("✓ enabled  web")
}

// Read only commands of a fresh home report their empty state in the words hermes uses
func TestScenarioFreshHomeReports(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("auth", "list").Exit(0).Has("openai-api (1 credentials):", "OPENAI_API_KEY", "api_key", "env:OPENAI_API_KEY").Lacks("sk-fake-e2e")
	s.Run("gateway", "status").Exit(0).Has("✗ Gateway is not running", "To start:", "icebath gateway run", "icebath gateway install")
	s.Run("insights").Exit(0).Has("No session data yet.")
	s.Run("hooks", "list").Exit(0).Has("No shell hooks or outbound webhooks configured in ~/.icebath/config.yaml.", "See `icebath hooks --help`")
	s.Run("pairing", "list").Exit(0).Has("No pairing data found. No one has tried to pair yet~")
	s.Run("checkpoints").Exit(0).Has("Checkpoint base: ${ICEBATH_HOME}/checkpoints", "Total size:", "Projects:        0")
	for shell, mark := range map[string]string{"bash": "# Icebath Agent bash completion", "zsh": "#compdef icebath", "fish": "complete -c icebath"} {
		script := s.Run("completion", shell).Exit(0)
		if !strings.Contains(script.Stdout, mark) || !strings.Contains(script.Stdout, "sessions") || !strings.Contains(script.Stdout, "gateway") {
			s.Failf("the %s completion script lacks %q or the command names\n%s", shell, mark, clip(script.Stdout))
		}
	}
}
