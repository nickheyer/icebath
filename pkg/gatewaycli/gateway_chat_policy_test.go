package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sets this shell's environment, allowlisted names and secrets alike, with the secret rules the process wiring installs
func clientShell(t *testing.T) {
	prevName, prevSubstrings := state.SecretEnvName, state.SecretEnvSubstrings
	state.SecretEnvName, state.SecretEnvSubstrings = codeexec.IsSecretEnvName, codeexec.SecretSubstrings
	t.Cleanup(func() { state.SecretEnvName, state.SecretEnvSubstrings = prevName, prevSubstrings })
	t.Setenv("PATH", "/client/venv/bin:/usr/bin")
	t.Setenv("VIRTUAL_ENV", "/client/venv")
	t.Setenv("ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "mine")
	t.Setenv("NODE_AUTH_TOKEN", "npm-secret")
	t.Setenv("OPENAI_API_KEY", "sk-secret")
}

// Fails unless the env holds the allowlisted shell names and none of the secrets
func expectClientEnv(t *testing.T, env map[string]any) {
	t.Helper()
	if env["PATH"] != "/client/venv/bin:/usr/bin" || env["VIRTUAL_ENV"] != "/client/venv" || env["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] != "mine" {
		t.Fatalf("client env %v", env)
	}
	if _, ok := env["NODE_AUTH_TOKEN"]; ok {
		t.Fatalf("secret forwarded: %v", env)
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatalf("secret forwarded: %v", env)
	}
}

// client_env of the policy for the parameters
func clientEnvOf(t *testing.T, parameters ...any) map[string]any {
	t.Helper()
	policy, err := ClientEnvPolicy(context.Background(), parameters)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := policy["client_env"].(map[string]any)
	return env
}

func TestClientEnvPolicyReadsTheProfilesExtraNames(t *testing.T) {
	clientShell(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("terminal: {client_env_passthrough: [MY_TOOLCHAIN_HOME]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MY_TOOLCHAIN_HOME", "/opt/tc")
	if policy, err := ClientEnvPolicy(context.Background(), []any{"source"}); err != nil || len(policy) != 0 {
		t.Fatalf("policy %v err %v", policy, err)
	}
	env := clientEnvOf(t, "client_env")
	expectClientEnv(t, env)
	if env["MY_TOOLCHAIN_HOME"] != "/opt/tc" {
		t.Fatalf("env %v", env)
	}
}

func TestClientEnvPolicyReadsTheEffectiveConfigAndBypassReadsNone(t *testing.T) {
	clientShell(t)
	root := t.TempDir()
	h, managed := filepath.Join(root, "home"), filepath.Join(root, "managed")
	for _, dir := range []string{h, managed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("terminal: {client_env_passthrough: [GEM_HOME]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEM_HOME", "/gems")
	for _, name := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	env := clientEnvOf(t, "client_env")
	expectClientEnv(t, env)
	if env["GEM_HOME"] != "/gems" {
		t.Fatalf("env %v", env)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("terminal: {client_env_passthrough: [MY_TOOLCHAIN_HOME]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MY_TOOLCHAIN_HOME", "/opt/tc")
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	bypass := clientEnvOf(t, "client_env")
	expectClientEnv(t, bypass)
	if _, ok := bypass["GEM_HOME"]; ok {
		t.Fatalf("bypass read the managed config: %v", bypass)
	}
	if _, ok := bypass["MY_TOOLCHAIN_HOME"]; ok {
		t.Fatalf("bypass read the profile config: %v", bypass)
	}
}

func TestResumeAcceptsTheCreationAliasAndStillRefusesAnotherModel(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	config := "model:\n  provider: custom\n  base_url: http://127.0.0.1:1/v1\n  default: model-main\n" +
		"model_aliases:\n  fast: {model: model-alias, provider: custom, base_url: http://127.0.0.1:3/v1}\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	provider.DirectAliases.Clear()
	t.Cleanup(provider.DirectAliases.Clear)
	frozen := map[string]any{"info": map[string]any{"launch_request": map[string]any{"source": "cli", "model": "model-alias",
		"provider": "custom", "base_url": "http://127.0.0.1:3/v1"}}}
	ctx := context.Background()
	for _, model := range []string{"fast", "model-alias"} {
		if err := CheckResumePolicy(ctx, map[string]any{"resume": "s", "model": model, "query": "x"}, frozen); err != nil {
			t.Fatalf("%s refused: %v", model, err)
		}
	}
	for _, model := range []string{"model-main", "other"} {
		var refused *gateway.GatewayClientError
		if err := CheckResumePolicy(ctx, map[string]any{"resume": "s", "model": model, "query": "x"}, frozen); !errors.As(err, &refused) {
			t.Fatalf("%s accepted: %v", model, err)
		}
	}
}

func TestClassicCLICreateSendsClientEnvWhenTheGatewayTakesIt(t *testing.T) {
	for _, takes := range []bool{true, false} {
		db, client := storeFixture(t)
		clientShell(t)
		parameters := []any{"cwd", "source", "row_cwd"}
		if takes {
			parameters = append(parameters, "client_env")
		}
		client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
			if method == "runtime.describe" {
				client.record(method, params)
				return map[string]any{"session_create": map[string]any{"sources": []any{"cli"}, "parameters": parameters}}, nil
			}
			return client.base(ctx, method, params)
		})
		if _, err := db.Create(context.Background(), map[string]any{}, "", false); err != nil {
			t.Fatal(err)
		}
		created := client.of("session.create")
		if len(created) != 1 {
			t.Fatalf("takes=%v: creates %v", takes, created)
		}
		env, sent := created[0]["client_env"]
		if takes {
			plain, _ := env.(map[string]any)
			expectClientEnv(t, plain)
		} else if sent {
			t.Fatalf("client_env sent to a gateway that does not take it: %v", created[0])
		}
	}
}
