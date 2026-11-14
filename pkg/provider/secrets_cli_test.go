package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Secrets CLI over a temp home writing to a buffer, every seam restored after the test
func newSecretsTestCLI(t *testing.T) (*secretsCLI, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	console := pyrich.NewConsole(&out)
	console.Width = 200
	cli := &secretsCLI{ctx: context.Background(), home: newTestHome(t), console: console}
	prevTTY, prevFind, prevVersion, prevLoad := stdinIsATTY, findBws, bwsVersion, secretsLoadConfig
	prevSave, prevPath, prevFetch, prevList := secretsSaveEnvValue, secretsEnvPath, fetchBitwardenSecrets, listProjects
	prevClear, prevFindOp := clearBitwardenCaches, findOp
	t.Cleanup(func() {
		stdinIsATTY, findBws, bwsVersion, secretsLoadConfig = prevTTY, prevFind, prevVersion, prevLoad
		secretsSaveEnvValue, secretsEnvPath, fetchBitwardenSecrets, listProjects = prevSave, prevPath, prevFetch, prevList
		clearBitwardenCaches, findOp = prevClear, prevFindOp
	})
	return cli, &out
}

func bwSetupNamespace(accessToken, serverURL, projectID string) map[string]any {
	return map[string]any{"access_token": accessToken, "server_url": serverURL, "project_id": projectID}
}

func TestCmdSetupNonTtyGuardMissingAccessTokenOnly(t *testing.T) {
	cli, out := newSecretsTestCLI(t)
	stdinIsATTY = func() bool { return false }
	findBws = func(bool) string { return "/usr/bin/bws" }
	bwsVersion = func(string) string { return "2.0.0" }
	code, err := bwCmdSetup(cli, bwSetupNamespace("", "https://vault.bitwarden.com", "aaaa-bbbb"))
	if err != nil || code != 1 {
		t.Fatalf("setup = %d %v", code, err)
	}
	captured := out.String()
	if !strings.Contains(captured, "Missing:") || !strings.Contains(captured, "--access-token") {
		t.Fatalf("output %q", captured)
	}
	missingLine := ""
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, "Missing:") {
			missingLine = line
			break
		}
	}
	if !strings.Contains(missingLine, "--access-token") || strings.Contains(missingLine, "--server-url") || strings.Contains(missingLine, "--project-id") {
		t.Fatalf("missing line %q", missingLine)
	}
}

func TestCmdSetupNonTtyGuardMissingServerUrlWithEnvVarPasses(t *testing.T) {
	cli, _ := newSecretsTestCLI(t)
	stdinIsATTY = func() bool { return false }
	t.Setenv("BWS_SERVER_URL", "https://vault.bitwarden.com")
	t.Setenv("BWS_ACCESS_TOKEN", "")
	findBws = func(bool) string { return "/usr/bin/bws" }
	bwsVersion = func(string) string { return "2.0.0" }
	secretsLoadConfig = func(string) (map[string]any, error) { return map[string]any{}, nil }
	secretsSaveEnvValue = func(string, string, string) error { return nil }
	secretsEnvPath = func(string) string { return "/tmp/.env" }
	fetchBitwardenSecrets = func(BitwardenFetchOptions) (map[string]string, []string, error) {
		return map[string]string{"KEY": "val"}, nil, nil
	}
	code, err := bwCmdSetup(cli, bwSetupNamespace("0.valid-token", "", "aaaa-bbbb"))
	if err != nil || code != 0 {
		t.Fatalf("setup = %d %v", code, err)
	}
}

// Bitwarden config and .env seams of the token rotation cases, the saved values collected
func bwEnv(t *testing.T) map[string]string {
	t.Helper()
	saved := map[string]string{}
	secretsLoadConfig = func(string) (map[string]any, error) {
		return map[string]any{"secrets": map[string]any{"bitwarden": map[string]any{
			"enabled": true, "access_token_env": "BWS_ACCESS_TOKEN", "project_id": "proj-1", "server_url": "",
		}}}, nil
	}
	secretsSaveEnvValue = func(_, name, value string) error {
		saved[name] = value
		return nil
	}
	secretsEnvPath = func(string) string { return filepath.Join(t.TempDir(), ".env") }
	findBws = func(bool) string { return "/fake/bws" }
	return saved
}

func TestBwTokenNoVerifySkipsProbe(t *testing.T) {
	cli, _ := newSecretsTestCLI(t)
	saved := bwEnv(t)
	t.Setenv("BWS_ACCESS_TOKEN", "")
	probed := false
	listProjects = func(*secretsCLI, string, string, *pyrich.Console, string) ([]map[string]any, bool) {
		probed = true
		return nil, false
	}
	clearBitwardenCaches = func(string) {}
	code, err := bwCmdToken(cli, map[string]any{"access_token": "0.x", "no_verify": true})
	if err != nil || code != 0 {
		t.Fatalf("token = %d %v", code, err)
	}
	if probed {
		t.Fatal("--no-verify still probed Bitwarden")
	}
	if len(saved) != 1 || saved["BWS_ACCESS_TOKEN"] != "0.x" {
		t.Fatalf("saved = %v", saved)
	}
}

// 1Password config and .env seams of the token rotation cases, the saved values collected
func opEnv(t *testing.T) map[string]string {
	t.Helper()
	saved := map[string]string{}
	secretsLoadConfig = func(string) (map[string]any, error) {
		return map[string]any{"secrets": map[string]any{"onepassword": map[string]any{
			"enabled": true, "service_account_token_env": "OP_SERVICE_ACCOUNT_TOKEN",
		}}}, nil
	}
	secretsSaveEnvValue = func(_, name, value string) error {
		saved[name] = value
		return nil
	}
	secretsEnvPath = func(string) string { return filepath.Join(t.TempDir(), ".env") }
	findOp = func(string) string { return "/fake/op" }
	return saved
}

func TestOpTokenNonTtyRequiresFlag(t *testing.T) {
	cli, _ := newSecretsTestCLI(t)
	saved := opEnv(t)
	stdinIsATTY = func() bool { return false }
	code, err := opCmdToken(cli, map[string]any{"token": "", "no_verify": false})
	if err != nil || code != 1 {
		t.Fatalf("token = %d %v", code, err)
	}
	if len(saved) != 0 {
		t.Fatalf("saved = %v", saved)
	}
}

func TestOpWhoamiSurvivesNonUtf8CliOutput(t *testing.T) {
	cli, _ := newSecretsTestCLI(t)
	dir := t.TempDir()
	script := "printf '\\377\\376 caf\\303\\251@example.com\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "whoami"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	identity, ok := opWhoami(cli, "sh", "", "")
	if !ok {
		t.Fatal("whoami reported no identity")
	}
	if !strings.Contains(identity, "café@example.com") {
		t.Fatalf("identity %q", identity)
	}
}
