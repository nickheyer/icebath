package scenario

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The version report names the product, its install and the Go runtime, and an unknown command is refused with exit 2
func TestScenarioVersionAndUnknownCommand(t *testing.T) {
	s := Start(t, Setup{})
	for _, flag := range []string{"--version", "-V"} {
		version := s.Run(flag).Exit(0)
		version.Has("Icebath Agent v", "Install directory: "+filepath.Dir(suite.bin), "Install method: ", "Go: ").Lacks("Python", "OpenAI SDK", "Hermes")
		if !strings.HasPrefix(version.Stdout, "Icebath Agent v") {
			s.Failf("the version report does not open with the product line\n%s", version.dump())
		}
	}
	unknown := s.Run("version").Exit(2)
	unknown.Has("icebath: 'version' is not a `icebath` command.", "Run `icebath --help` to see all commands.")
	if unknown.Stdout != "" {
		s.Failf("an unknown command writes to standard error only\n%s", unknown.dump())
	}
}

// Status and dump describe the configured model, provider, keys and features without printing any secret
func TestScenarioStatusAndDump(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{".env": "OPENAI_API_KEY=sk-scenario-not-shown-0123456789\n"}})
	status := s.Run("status").Exit(0)
	status.Has("Icebath Agent Status", "Model:", "fake-model", "Provider:", "custom", "Gateway:", "stopped", "Platforms:", "none configured",
		"Jobs:", "Run 'icebath status --full' for every section").Lacks("sk-scenario-not-shown")

	dump := s.Run("dump").Exit(0)
	dump.Has("--- icebath dump ---", "--- end dump ---", "api_keys:", "features:").Lacks("sk-scenario-not-shown", "python", "openai_sdk")
	for _, line := range []string{`version:\s+\S.*`, `os:\s+Linux .+`, `go:\s+\S.*`, `profile:\s+default`, `icebath_home:\s+` + regexp.QuoteMeta(s.Home),
		`model:\s+fake-model`, `provider:\s+custom`, `terminal:\s+local`, `\s+openai\s+set`, `\s+openrouter\s+not set`, `\s+anthropic\s+not set`,
		`\s+mcp_servers:\s+0`, `\s+memory_provider:\s+built-in`, `\s+platforms:\s+none`, `\s+cron_jobs:\s+0`} {
		if !regexp.MustCompile(`(?m)^` + line + `$`).MatchString(dump.Stdout) {
			s.Failf("dump lacks a line matching %s\n%s", line, dump.dump())
		}
	}
	s.Run("cron", "create", "--name", "nightly", "0 3 * * *", "tidy up").Exit(0)
	s.Run("dump").Exit(0).Match(`(?m)^\s+cron_jobs:\s+1`)
	s.Run("status").Exit(0).Match(`Jobs:\s+1`)
}

// Doctor reports the sections it checked and holds no complaint about an interpreter icebath does not need
func TestScenarioDoctorHealthyHome(t *testing.T) {
	s := Start(t, Setup{})
	s.Finding("SF-4")
	s.Run("config", "migrate").Exit(0)
	doctor := s.RunStep(Step{Args: []string{"doctor"}, Timeout: 4 * time.Minute})
	doctor.Has("Icebath Doctor", "◆ Security Advisories", "No active security advisories", "◆ Configuration Files",
		"${ICEBATH_HOME}/config.yaml exists", "${ICEBATH_HOME}/.env file exists", "API key or custom endpoint configured", "◆ Directory Structure")
	doctor.Lacks("✗ Python (missing)", "Repair Python", "virtual environment", "Hermes")
}
