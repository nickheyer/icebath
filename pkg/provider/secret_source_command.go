package provider

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// Tight on purpose, a helper must be fast and non interactive
	commandSecretTimeout   = 3 * time.Second
	commandSecretMaxOutput = 1024 * 1024
)

// Anchored so a multi line blob never matches
var envLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

func commandSecretLog(message string) { fmt.Fprintf(os.Stderr, "[secrets:command] %s\n", message) }

// UnquoteDotenvValue strips one layer of matching surrounding quotes from a dotenv value
func UnquoteDotenvValue(raw string) string {
	t := strings.TrimSpace(raw)
	if len(t) >= 2 && t[0] == t[len(t)-1] && (t[0] == '"' || t[0] == '\'') {
		return t[1 : len(t)-1]
	}
	return t
}

// Parses a KEY=VALUE blob, comments and non env shaped lines are skipped
func parseDotenvMap(stdout string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := envLineRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = UnquoteDotenvValue(m[2])
		}
	}
	return out
}

// CommandSource is a user configured helper command as a bulk source
type CommandSource struct{}

func (s *CommandSource) Spec() SecretSourceSpec {
	return SecretSourceSpec{APIVersion: SecretSourceAPIVersion, Name: "command", Label: "Command helper", Shape: "bulk",
		RemediationHints: map[SecretErrorKind]string{
			SecretErrorNotConfigured: "Set secrets.command.command in config.yaml to a fast, non-interactive helper that prints KEY=VALUE lines.",
			SecretErrorInternal:      "Run the helper manually in a shell to see its real error — Icebath discards helper stderr so diagnostics can't leak secret material.",
		}}
}

func (s *CommandSource) ConfigSchema() map[string]map[string]any {
	return map[string]map[string]any{
		"enabled":                {"description": "Master switch", "default": false},
		"command":                {"description": "Helper run via /bin/sh -c; must print a KEY=VALUE blob on stdout", "default": ""},
		"helper_timeout_seconds": {"description": "Hard timeout for one helper run", "default": commandSecretTimeout.Seconds()},
		"override_existing":      {"description": "Helper values overwrite .env/shell values", "default": false},
	}
}

func (s *CommandSource) Fetch(cfg map[string]any, homePath string, environ *SourceEnvironment) *SecretFetchResult {
	result := NewSecretFetchResult()
	command := strings.TrimSpace(anyString(cfg["command"]))
	if command == "" {
		return result.Fail("secrets.command.enabled is true but secrets.command.command is empty.  Set the helper command in config.yaml.", SecretErrorNotConfigured)
	}
	if runtime.GOOS == "windows" {
		return result.Fail("the 'command' secret source is POSIX-only (needs /bin/sh); skipping on Windows", SecretErrorNotConfigured)
	}
	timeout := CoerceFloat(cfg["helper_timeout_seconds"], commandSecretTimeout.Seconds())
	if _, ok := cfg["helper_timeout_seconds"]; !ok {
		timeout = commandSecretTimeout.Seconds()
	}
	stdout, ok := runSecretHelper(command, "", time.Duration(timeout*float64(time.Second)), commandSecretMaxOutput, environ)
	if !ok {
		return result.Fail("helper command failed (see structured fields above); no secrets applied", SecretErrorInternal)
	}
	secrets := parseDotenvMap(stdout)
	if len(secrets) == 0 {
		result.Warnings = append(result.Warnings, "helper output was not a KEY=VALUE map; nothing to apply")
		return result
	}
	result.Secrets = secrets
	return result
}
