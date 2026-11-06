package codeexec

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Client toolchain a local shell session attaches with
var sessionClient = map[string]string{
	"PATH":        strings.Join([]string{"/client/venv/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)),
	"VIRTUAL_ENV": "/client/venv",
	"NVM_DIR":     "/client/.nvm",
}

// Secret and control names execute_code refuses even under a built in prefix or a configured extra name
var refusedClientEnv = map[string]string{"NODE_BEARER": "b", "NODE_CREDS": "c", "CONDA_DSN": "postgres://u:p@h/db",
	"DEPLOY_WEBHOOK": "https://h", "ICEBATH_KANBAN_DB": "/board.db", "ICEBATH_DELEGATED_CHILD_CONTEXT": "1"}

// Gives the process the daemon's own toolchain with no venv or nvm markers
func useDaemonEnv(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/daemon/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	unsetEnv(t, "VIRTUAL_ENV")
	unsetEnv(t, "NVM_DIR")
}

// Installs the session toolchain hooks bound to one client scope with the secret rule the process wiring installs
func bindSessionClient(t *testing.T, client map[string]string, local bool) {
	ctx := context.Background()
	if client != nil {
		ctx = state.ClientEnvScope(ctx, client, local, nil)
	}
	prevToolchain, prevTool, prevSecret, prevSubstrings := tools.SessionToolchainEnviron, tools.SessionToolEnv, state.SecretEnvName, state.SecretEnvSubstrings
	state.SecretEnvName, state.SecretEnvSubstrings = IsSecretEnvName, SecretSubstrings
	tools.SessionToolchainEnviron = func(_ *tools.Env, base map[string]string) map[string]string { return state.ToolchainEnviron(ctx, base) }
	tools.SessionToolEnv = func(*tools.Env) map[string]string { return state.ToolEnv(ctx) }
	t.Cleanup(func() {
		tools.SessionToolchainEnviron, tools.SessionToolEnv = prevToolchain, prevTool
		state.SecretEnvName, state.SecretEnvSubstrings = prevSecret, prevSubstrings
	})
}

func sessionChildEnv(t *testing.T, env *tools.Env, rt interpreter) map[string]string {
	t.Helper()
	child, err := buildChildEnv(t.Context(), env, childEnvSpec{RPCEndpoint: "sock", RPCToken: "t", TmpDir: t.TempDir(), ChildPython: rt.Python})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func pathParts(env map[string]string) []string {
	return strings.Split(env["PATH"], string(os.PathListSeparator))
}

func TestToolEnvsCarryTheBehaviourValuesTheSessionUses(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	useDaemonEnv(t)
	for _, name := range state.ClientBehaviourEnv {
		t.Setenv(name, "daemon-"+name)
	}
	client := maps.Clone(sessionClient)
	client["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] = "client prompt"
	bindSessionClient(t, client, true)
	child := sessionChildEnv(t, env, rt)
	for _, name := range state.ClientBehaviourEnv {
		if _, ok := child[name]; ok {
			t.Fatalf("behaviour value %s reached the execute_code child: %v", name, child)
		}
	}
}

func TestExecuteCodeChildEnvStartsFromTheSessionsToolchain(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	useDaemonEnv(t)
	bindSessionClient(t, sessionClient, true)
	bound := sessionChildEnv(t, env, rt)
	if !slices.Contains(pathParts(bound), "/client/venv/bin") || bound["VIRTUAL_ENV"] != "/client/venv" {
		t.Fatalf("bound child env %v", bound)
	}
	bindSessionClient(t, nil, true)
	if unbound := sessionChildEnv(t, env, rt); !slices.Contains(pathParts(unbound), "/daemon/bin") {
		t.Fatalf("unbound child env %v", unbound)
	}
}

func TestCredentialAndControlNamesAreRefusedByExecuteCodesSecretPolicy(t *testing.T) {
	bindSessionClient(t, nil, true)
	cfg := map[string]any{"terminal": map[string]any{"client_env_passthrough": []any{"DEPLOY_WEBHOOK", "ICEBATH_KANBAN_DB", "MY_TOOLCHAIN_HOME"}}}
	sent := maps.Clone(refusedClientEnv)
	kept := map[string]string{"NVM_DIR": "/client/.nvm", "MY_TOOLCHAIN_HOME": "/opt/tc", "PATH": "/client/bin"}
	maps.Copy(sent, kept)
	accepted, err := state.AcceptClientEnv(anyMap(sent), cfg, true)
	if err != nil || !maps.Equal(accepted, kept) {
		t.Fatalf("gateway accepted %v err %v", accepted, err)
	}
	if collected := state.CollectClientEnv(sent, cfg); !maps.Equal(collected, kept) {
		t.Fatalf("client collected %v", collected)
	}
}

func anyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestExecuteCodeScrubsAfterMergingTheSessionsToolchain(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	useDaemonEnv(t)
	client := maps.Clone(sessionClient)
	maps.Copy(client, refusedClientEnv)
	bindSessionClient(t, client, true)
	child := sessionChildEnv(t, env, rt)
	if child["NVM_DIR"] != "/client/.nvm" || !slices.Contains(pathParts(child), "/client/venv/bin") {
		t.Fatalf("toolchain lost %v", child)
	}
	for name := range refusedClientEnv {
		if _, ok := child[name]; ok {
			t.Fatalf("%s reached the sandbox", name)
		}
	}
}
