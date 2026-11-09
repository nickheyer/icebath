package selfupdate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Isolated home with the project root pointed at a directory that has no git and no .env
func dumpHome(t *testing.T) (context.Context, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"OPENROUTER_API_KEY", "FIRECRAWL_API_KEY", "KEENABLE_API_KEY", "TAVILY_API_KEY", "TERMINAL_ENV"} {
		t.Setenv(name, "")
	}
	prev := dumpProjectRoot
	dumpProjectRoot = func() string { return filepath.Join(tmp, "noproject") }
	t.Cleanup(func() { dumpProjectRoot = prev })
	return home.WithOverride(context.Background(), h), h
}

// Runs the dump and returns its output
func dumpOutput(t *testing.T, ctx context.Context, h string) string {
	t.Helper()
	var buf bytes.Buffer
	if rc, err := RunDump(ctx, h, map[string]any{"show_keys": false}, &buf); err != nil || rc != 0 {
		t.Fatalf("RunDump %d %v", rc, err)
	}
	return buf.String()
}

// Line of the api_keys block for a label
func dumpAPIKeyLine(t *testing.T, out, label string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), label+" ") {
			return line
		}
	}
	t.Fatalf("no %q api_keys line in dump output:\n%s", label, out)
	return ""
}

func TestDumpFlagsShellOnlyKeyNotInDotenv(t *testing.T) {
	ctx, h := dumpHome(t)
	if err := os.WriteFile(home.EnvPath(h), []byte("OPENROUTER_API_KEY=sk-or-xxxx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRECRAWL_API_KEY", "fc-shell-only")
	line := dumpAPIKeyLine(t, dumpOutput(t, ctx, h), "firecrawl")
	if !strings.Contains(line, "set") || !strings.Contains(line, "shell only") || !strings.Contains(line, ".env") {
		t.Fatalf("%q", line)
	}
}

func TestDumpLeavesUnsetKeyUntouched(t *testing.T) {
	ctx, h := dumpHome(t)
	if err := os.WriteFile(home.EnvPath(h), []byte("OPENROUTER_API_KEY=sk-or-xxxx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := dumpAPIKeyLine(t, dumpOutput(t, ctx, h), "keenable")
	if !strings.Contains(line, "not set") || strings.Contains(line, "shell only") {
		t.Fatalf("%q", line)
	}
}

func TestS6SupervisedGatewayWithoutPidIsRunningNotUnknown(t *testing.T) {
	prev := dumpGatewaySnapshot
	dumpGatewaySnapshot = func(context.Context) (gatewaycli.GatewayRuntimeSnapshot, error) {
		return gatewaycli.GatewayRuntimeSnapshot{Manager: "s6 (container supervisor)", ServiceInstalled: true, ServiceRunning: true}, nil
	}
	t.Cleanup(func() { dumpGatewaySnapshot = prev })
	if got := dumpGatewayStatus(context.Background()); got != "running (s6 (container supervisor))" {
		t.Fatalf("%q", got)
	}
}

// Swaps the version info and code identity seams the way the hermes tests patch version_info
func dumpSwapVersion(t *testing.T, info updaterecord.VersionInfo) {
	t.Helper()
	prevInfo, prevIdentity := dumpVersionInfo, dumpCodeIdentity
	dumpVersionInfo = func() (updaterecord.VersionInfo, error) { return info, nil }
	dumpCodeIdentity = func(bool) (*pyjson.Object, error) {
		identity := pyjson.NewObject()
		if info.Commit != nil {
			identity.Set("short_sha", (*info.Commit)[:8])
		}
		return identity, nil
	}
	t.Cleanup(func() { dumpVersionInfo, dumpCodeIdentity = prevInfo, prevIdentity })
}

func TestDumpUsesRunningInstallFallbackOnly(t *testing.T) {
	sha := strings.Repeat("cafef00d", 5)
	stamp := int64(1718662620)
	cases := []struct {
		name     string
		commit   *string
		date     *int64
		expected string
		want     string
	}{
		{"stamped", &sha, &stamp, "cafef00d", "2024-06-17"},
		{"unstamped", nil, nil, "(unknown)", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			prev := dumpProjectRoot
			dumpProjectRoot = func() string { return tmp }
			t.Cleanup(func() { dumpProjectRoot = prev })
			dumpSwapVersion(t, updaterecord.VersionInfo{BaseVersion: "1.0", DerivedVersion: "1.0", Commit: tc.commit, Source: "docker", CommitDate: tc.date})
			if got := dumpGetGitCommit(tmp); got != tc.expected {
				t.Fatalf("commit %q", got)
			}
			if got := dumpGetGitCommitDate(tmp); got != tc.want {
				t.Fatalf("date %q", got)
			}
		})
	}
}

func TestDumpVersionLineUsesDerivedRuntimeVersion(t *testing.T) {
	commit, distance, branch := strings.Repeat("abcdef0", 5)+"abcde", int64(4), "main"
	dumpSwapVersion(t, updaterecord.VersionInfo{BaseVersion: "1.2.3", DerivedVersion: "1.2.3+4.gabcdef0", Distance: &distance, Commit: &commit, Branch: &branch, Source: "git"})
	prevCommit, prevDate := dumpGitCommitFn, dumpGitCommitDateFn
	dumpGitCommitFn = func(string) string { return "abcdef0" }
	dumpGitCommitDateFn = func(string) string { return "" }
	t.Cleanup(func() { dumpGitCommitFn, dumpGitCommitDateFn = prevCommit, prevDate })
	if got := dumpVersionLine(t.TempDir()); got != "1.2.3+4.gabcdef0 [abcdef0]" {
		t.Fatalf("%q", got)
	}
}

func TestGetGitCommitUnknownForOtherRootWithoutGit(t *testing.T) {
	other := filepath.Join(t.TempDir(), "plain-dir")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := dumpGetGitCommit(other); got != "(unknown)" {
		t.Fatalf("%q", got)
	}
}

// Runs git in dir with a fixture identity
func dumpGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGetGitCommitAuthoritativeForRealTempRepo(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "scratch-repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dumpGit(t, repo, "init", "-b", "main")
	dumpGit(t, repo, "add", ".")
	dumpGit(t, repo, "commit", "-m", "scratch", "--no-gpg-sign")
	expected := dumpGit(t, repo, "rev-parse", "--short=8", "HEAD")
	commit := dumpGetGitCommit(repo)
	if commit != expected {
		t.Fatalf("commit %q want %q", commit, expected)
	}
	if commit == dumpGetGitCommit(dumpProjectRoot()) {
		t.Fatal("the temp repo answered with the running install's commit")
	}
}

func TestGetGitCommitDateUnknownForOtherRootWithoutGit(t *testing.T) {
	other := filepath.Join(t.TempDir(), "plain-dir")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := dumpGetGitCommitDate(other); got != "" {
		t.Fatalf("%q", got)
	}
}

// Writes the config and .env of a home
func dumpSeed(t *testing.T, h, configYAML, envText string) {
	t.Helper()
	if err := os.WriteFile(home.ConfigPath(h), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home.EnvPath(h), []byte(envText), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Terminal line of the dump output
func dumpTerminalLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "terminal:") {
			return line
		}
	}
	t.Fatalf("no terminal line in dump output:\n%s", out)
	return ""
}

func TestDumpSurfacesTerminalEnvOverride(t *testing.T) {
	ctx, h := dumpHome(t)
	t.Setenv("TERMINAL_ENV", "docker")
	dumpSeed(t, h, "display:\n  streaming: true\n", "")
	line := dumpTerminalLine(t, dumpOutput(t, ctx, h))
	if !strings.Contains(line, "docker") || !strings.Contains(line, "overrides config.yaml") || !strings.Contains(line, "terminal.backend=local") {
		t.Fatalf("%q", line)
	}
}

func TestDumpReportsConfigBackendWinningOverStaleEnv(t *testing.T) {
	ctx, h := dumpHome(t)
	dumpSeed(t, h, "terminal:\n  backend: local\n", "TERMINAL_ENV=docker\n")
	line := dumpTerminalLine(t, dumpOutput(t, ctx, h))
	if !strings.Contains(line, "local") || strings.Contains(line, "overrides") {
		t.Fatalf("%q", line)
	}
}

func TestDumpReportsConfigBackendWhenNoOverride(t *testing.T) {
	ctx, h := dumpHome(t)
	dumpSeed(t, h, "terminal:\n  backend: docker\n", "")
	line := dumpTerminalLine(t, dumpOutput(t, ctx, h))
	if !strings.Contains(line, "docker") || strings.Contains(line, "overrides") {
		t.Fatalf("%q", line)
	}
}

func TestRedactDebugSupportTextScrubsParamsHeadersArgvAndEmails(t *testing.T) {
	cases := map[string]string{
		"https://h/x?api_key=abc123&ok=1":                      "https://h/x?api_key=***&ok=1",
		"u=https%3A%2F%2Fh%2Fx%3Ftoken%3Dabc123%26a%3D1":       "u=https%3A%2F%2Fh%2Fx%3Ftoken%3D%2A%2A%2A%26a%3D1",
		"Authorization: Bearer sk-abcdefghijklmnop":            "Authorization: [REDACTED]",
		"'authorization': 'abc'":                               "'authorization': '[REDACTED]'",
		"--api-key=zq9secretvalue123456 --flag":                "--api-key=[REDACTED] --flag",
		"'--token', 'abcdef123456'":                            "'--token', '[REDACTED]'",
		"'--header', 'authorization', 'Bearer', 'abc'":         "'--header', 'authorization', 'Bearer', '[REDACTED]'",
		"wrote to alice.smith@example.com and bob@example.org": "wrote to [REDACTED_EMAIL] and [REDACTED_EMAIL]",
		"Bearer realm=example":                                 "Bearer realm=example",
		"":                                                     "",
	}
	for in, want := range cases {
		if got := RedactDebugSupportText(in); got != want {
			t.Fatalf("%q -> %q want %q", in, got, want)
		}
	}
	if got := RedactDebugSupportTextCapped("plain text with no secrets", 5); got != "plain" {
		t.Fatalf("capped %q", got)
	}
	if got := RedactDebugSupportText(nil); got != "" {
		t.Fatalf("nil %q", got)
	}
}

func TestDumpFallbackProjectionKeepsRouteMetadataOnly(t *testing.T) {
	projected := dumpFallbackDebugProjection([]any{map[string]any{"provider": "openrouter", "model": "m", "api_key": "sk-x", "headers": map[string]any{"x": "y"}, "max_tokens": 7, "base_url": 5}, "junk"})
	if len(projected) != 1 {
		t.Fatalf("%v", projected)
	}
	item, _ := pyjson.AsObject(projected[0])
	if item.Vals["provider"] != "openrouter" || item.Vals["max_tokens"] != 7 || item.Vals["credential_source"] != "inline" {
		t.Fatalf("%v", item.Vals)
	}
	if _, has := item.Vals["headers"]; has {
		t.Fatal("headers projected")
	}
	if _, has := item.Vals["base_url"]; has {
		t.Fatal("a non string base_url projected")
	}
}
