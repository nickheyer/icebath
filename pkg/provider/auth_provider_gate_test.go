package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Unsets each variable for the test, restoring it afterwards
func unsetEnvForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// Fresh home without Anthropic env credentials, returning the home dir
func gateHome(t *testing.T) string {
	t.Helper()
	h := newTestHome(t)
	unsetEnvForTest(t, "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN")
	return h
}

func writeGateAuthStore(t *testing.T, h, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const gateEmptyStore = `{"version": 1, "providers": {}, "active_provider": null}`

func explicitIn(ctx context.Context, provider string) bool {
	return IsProviderExplicitlyConfigured(modelsPoolEnv(ctx), provider)
}

func explicitlyConfigured(provider string) bool { return explicitIn(context.Background(), provider) }

func TestAmbientPoolSourceDoesNotCountAsExplicit(t *testing.T) {
	h := gateHome(t)
	unsetEnvForTest(t, "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
	writeGateAuthStore(t, h, `{"version": 1, "providers": {}, "active_provider": null, "credential_pool": {"copilot": [
		{"id": "abc123", "source": "gh_cli", "auth_type": "api_key", "access_token": "ghu_sometoken"}]}}`)
	if explicitlyConfigured("copilot") {
		t.Fatal("gh_cli seeded pool entry counted as explicit")
	}
}

func TestVertexADCCountsAsExplicitWhenConfigPresent(t *testing.T) {
	h := gateHome(t)
	unsetEnvForTest(t, "VERTEX_PROJECT_ID", "VERTEX_CREDENTIALS_PATH", "GOOGLE_APPLICATION_CREDENTIALS")
	writeGateAuthStore(t, h, gateEmptyStore)
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\nvertex:\n  project_id: my-gcp-project\n")
	if !explicitlyConfigured("vertex") {
		t.Fatal("vertex.project_id did not count")
	}
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\n")
	if explicitlyConfigured("vertex") {
		t.Fatal("vertex counted without scoped config")
	}
}

func TestVertexAmbientGoogleCredsEnvDoesNotCountAsExplicit(t *testing.T) {
	h := gateHome(t)
	unsetEnvForTest(t, "VERTEX_PROJECT_ID", "VERTEX_CREDENTIALS_PATH")
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\n")
	writeGateAuthStore(t, h, gateEmptyStore)
	sa := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(sa, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", sa)
	if explicitlyConfigured("vertex") {
		t.Fatal("ambient GOOGLE_APPLICATION_CREDENTIALS counted")
	}
}

func TestVertexCredentialsPathMustBeReadableFile(t *testing.T) {
	h := gateHome(t)
	unsetEnvForTest(t, "VERTEX_PROJECT_ID", "GOOGLE_APPLICATION_CREDENTIALS")
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\n")
	writeGateAuthStore(t, h, gateEmptyStore)
	dir := t.TempDir()
	saFile := filepath.Join(dir, "vertex_sa.json")
	if err := os.WriteFile(saFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	saDir := filepath.Join(dir, "vertex_dir")
	if err := os.Mkdir(saDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{saFile: true, saDir: false, filepath.Join(dir, "nonexistent.json"): false} {
		t.Setenv("VERTEX_CREDENTIALS_PATH", path)
		if explicitlyConfigured("vertex") != want {
			t.Fatalf("%s want %v", path, want)
		}
	}
}

func TestBedrockRegionCountsAsExplicit(t *testing.T) {
	h := gateHome(t)
	writeGateAuthStore(t, h, gateEmptyStore)
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\nbedrock:\n  region: us-east-1\n")
	if !explicitlyConfigured("bedrock") {
		t.Fatal("bedrock region did not count")
	}
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  default: claude-opus-4-8\nbedrock:\n  region: ''\n")
	if explicitlyConfigured("bedrock") {
		t.Fatal("empty bedrock region counted")
	}
}

func TestReturnsTrueWhenMoAReferenceSlotUsesProvider(t *testing.T) {
	h := gateHome(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\nmoa:\n  presets:\n    default:\n      reference_models:\n"+
		"        - provider: anthropic\n          model: claude-opus-4-8\n        - provider: opencode-go\n          model: glm-5.2\n"+
		"      aggregator:\n        provider: openai-codex\n        model: gpt-5.5\n")
	writeGateAuthStore(t, h, `{"version": 1, "providers": {}, "active_provider": "openai-codex"}`)
	if !explicitlyConfigured("anthropic") {
		t.Fatal("moa reference slot provider did not count")
	}
}

func TestStaleEnvPoolEntryDoesNotCountWhenVarUnset(t *testing.T) {
	h := gateHome(t)
	unsetEnvForTest(t, "DEEPSEEK_API_KEY")
	writeGateAuthStore(t, h, `{"version": 1, "providers": {}, "active_provider": null, "credential_pool": {"deepseek": [
		{"id": "aaa111", "source": "env:DEEPSEEK_API_KEY", "auth_type": "api_key"}]}}`)
	if explicitlyConfigured("deepseek") {
		t.Fatal("stale env pool entry counted")
	}
}

func TestProfileDotenvKeyCountsAsExplicitWhenProcessEnvLacksIt(t *testing.T) {
	root := gateHome(t)
	studio := filepath.Join(root, "profiles", "content-studio")
	if err := os.MkdirAll(studio, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfigYAML(t, root, "model: {}\n")
	writeConfigYAML(t, studio, "model: {}\n")
	if err := os.WriteFile(filepath.Join(studio, ".env"), []byte("DEEPSEEK_API_KEY=sk-studio-deepseek-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsetEnvForTest(t, "DEEPSEEK_API_KEY")
	if explicitlyConfigured("deepseek") {
		t.Fatal("deepseek explicit outside the studio profile")
	}
	if !explicitIn(home.WithOverride(context.Background(), studio), "deepseek") {
		t.Fatal("studio profile dotenv key did not count")
	}
	if explicitIn(home.WithOverride(context.Background(), root), "deepseek") {
		t.Fatal("deepseek explicit after leaving the studio profile")
	}
}

func TestDotenvKeyCountsWhenShellExportsTheVarEmpty(t *testing.T) {
	h := gateHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	writeConfigYAML(t, h, "model: {}\n")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("DEEPSEEK_API_KEY=sk-dotenv-only-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, _ := apiKeyCredentials(context.Background(), "deepseek"); key != "sk-dotenv-only-secret" {
		t.Fatalf("resolved key %q", key)
	}
	if !explicitlyConfigured("deepseek") {
		t.Fatal("dotenv key under an empty shell export did not count")
	}
}

func TestBedrockAWSEnvGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		provider string
		want     bool
	}{
		{"no aws env", nil, "bedrock", false},
		{"bearer token", map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "ABSKexample-bearer-token-value"}, "bedrock", true},
		{"access key pair", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE1234567890",
			"AWS_SECRET_ACCESS_KEY": "examplesecretexamplesecretexample0000000"}, "bedrock", true},
		{"access key without secret", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE1234567890"}, "bedrock", false},
		{"ambient aws profile", map[string]string{"AWS_PROFILE": "default"}, "bedrock", false},
		{"aws env does not leak", map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "ABSKexample-bearer-token-value"}, "anthropic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateHome(t)
			unsetEnvForTest(t, "AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
				"AWS_PROFILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_WEB_IDENTITY_TOKEN_FILE")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if explicitlyConfigured(tc.provider) != tc.want {
				t.Fatalf("%s want %v", tc.provider, tc.want)
			}
		})
	}
}
