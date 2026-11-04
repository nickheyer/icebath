package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
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

// Runs the helper through sh -c with the key as data in ICEBATH_SECRET_KEY, stdout or false
func runSecretHelper(command, secretKey string, timeout time.Duration, maxOutput int, environ *SourceEnvironment) (string, bool) {
	if runtime.GOOS == "windows" {
		commandSecretLog("the 'command' provider is POSIX-only (needs /bin/sh); resolving no value on Windows")
		return "", false
	}
	env := SourceChildEnv(environ)
	env["ICEBATH_SECRET_KEY"] = secretKey
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = envList(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		errno := "unknown"
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			if en, ok := pathErr.Err.(syscall.Errno); ok {
				errno = fmt.Sprint(int(en))
			}
		}
		commandSecretLog("helper failed to spawn; resolving no value: errno=" + errno)
		return "", false
	}
	err := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		commandSecretLog(fmt.Sprintf("helper timed out after %gs; resolving no value", timeout.Seconds()))
		return "", false
	}
	if err != nil {
		code, signame := "?", "none"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signame = status.Signal().String()
			} else {
				code = fmt.Sprint(exitErr.ExitCode())
			}
		}
		commandSecretLog(fmt.Sprintf("helper failed; resolving no value: code=%s signal=%s", code, signame))
		return "", false
	}
	if stdout.Len() > maxOutput {
		commandSecretLog(fmt.Sprintf("helper output exceeded the %d-byte cap; resolving no value", maxOutput))
		return "", false
	}
	return strings.ToValidUTF8(stdout.String(), "�"), true
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
