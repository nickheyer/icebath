package tools

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Client toolchain a local shell session attaches with
var sessionClientEnv = map[string]string{
	"PATH":        strings.Join([]string{"/client/venv/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)),
	"VIRTUAL_ENV": "/client/venv",
	"NVM_DIR":     "/client/.nvm",
}

// Gives the process the daemon's own toolchain with no venv or nvm markers
func useDaemonEnv(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/daemon/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	for _, name := range []string{"VIRTUAL_ENV", "NVM_DIR"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Installs the session toolchain hooks bound to one client scope, an unbound context when client is nil
func bindSessionClient(t *testing.T, client map[string]string, local bool) {
	ctx := context.Background()
	if client != nil {
		ctx = state.ClientEnvScope(ctx, client, local, nil)
	}
	prevToolchain, prevTool := SessionToolchainEnviron, SessionToolEnv
	SessionToolchainEnviron = func(_ *Env, base map[string]string) map[string]string { return state.ToolchainEnviron(ctx, base) }
	SessionToolEnv = func(*Env) map[string]string { return state.ToolEnv(ctx) }
	t.Cleanup(func() {
		SessionToolchainEnviron, SessionToolEnv = prevToolchain, prevTool
	})
}

// PATH entries of a child env
func childPathParts(env map[string]string) []string {
	return strings.Split(env["PATH"], string(os.PathListSeparator))
}

// Builds the terminal run env over childEnv and fails the test on error
func sessionRunEnv(t *testing.T, env *Env, childEnv map[string]string) map[string]string {
	t.Helper()
	out, err := makeRunEnv(env, childEnv)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Builds the background process env and fails the test on error
func mustSpawnEnv(t *testing.T, env *Env) map[string]string {
	t.Helper()
	out, err := (&ProcessRegistry{}).spawnEnv(env, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A bound session's terminal starts from its client's toolchain beneath the backend env, an unbound one from the daemon's
func TestTerminalRunEnvStartsFromTheSessionsToolchain(t *testing.T) {
	env := newTestToolEnv(t)
	useDaemonEnv(t)
	bindSessionClient(t, sessionClientEnv, true)
	run := sessionRunEnv(t, env, map[string]string{})
	if !slices.Contains(childPathParts(run), "/client/venv/bin") || slices.Contains(childPathParts(run), "/daemon/bin") || run["NVM_DIR"] != "/client/.nvm" {
		t.Fatalf("bound run env PATH %q NVM_DIR %q", run["PATH"], run["NVM_DIR"])
	}
	if got := sessionRunEnv(t, env, map[string]string{"NVM_DIR": "/configured"})["NVM_DIR"]; got != "/configured" {
		t.Fatalf("backend env lost to the client %q", got)
	}
	bindSessionClient(t, map[string]string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "p"}, true)
	if _, ok := sessionRunEnv(t, env, map[string]string{})["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"]; ok {
		t.Fatal("behaviour name reached the tool env")
	}
	bindSessionClient(t, nil, true)
	run = sessionRunEnv(t, env, map[string]string{})
	if _, ok := run["NVM_DIR"]; ok || !slices.Contains(childPathParts(run), "/daemon/bin") {
		t.Fatalf("unbound run env PATH %q NVM_DIR %q", run["PATH"], run["NVM_DIR"])
	}
}

// Shell clients keep the daemon's behaviour values out of tool children, remote clients keep the host prefill file
func TestToolEnvsCarryTheBehaviourValuesTheSessionUses(t *testing.T) {
	env := newTestToolEnv(t)
	useDaemonEnv(t)
	daemon := map[string]string{}
	for _, name := range state.ClientBehaviourEnv {
		daemon[name] = "daemon-" + name
		t.Setenv(name, daemon[name])
	}
	client := map[string]string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "client prompt"}
	for k, v := range sessionClientEnv {
		client[k] = v
	}
	bindSessionClient(t, client, true)
	for i, child := range []map[string]string{sessionRunEnv(t, env, map[string]string{}), mustSpawnEnv(t, env)} {
		for _, name := range state.ClientBehaviourEnv {
			if _, ok := child[name]; ok {
				t.Fatalf("env %d carries behaviour name %s", i, name)
			}
		}
	}
	bindSessionClient(t, sessionClientEnv, false)
	terminal := sessionRunEnv(t, env, map[string]string{})
	_, prompt := terminal["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"]
	_, provider := terminal["ICEBATH_INFERENCE_PROVIDER"]
	if prompt || provider || terminal["ICEBATH_PREFILL_MESSAGES_FILE"] != daemon["ICEBATH_PREFILL_MESSAGES_FILE"] {
		t.Fatalf("remote client terminal behaviour values %q", terminal)
	}
	bindSessionClient(t, nil, true)
	if got := sessionRunEnv(t, env, map[string]string{})["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"]; got != daemon["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] {
		t.Fatalf("unbound terminal prompt %q", got)
	}
}

// Background processes start from the bound client's toolchain and the daemon's when unbound
func TestBackgroundProcessEnvStartsFromTheSessionsToolchain(t *testing.T) {
	env := newTestToolEnv(t)
	useDaemonEnv(t)
	bindSessionClient(t, sessionClientEnv, true)
	bound := mustSpawnEnv(t, env)
	if !slices.Contains(childPathParts(bound), "/client/venv/bin") || bound["NVM_DIR"] != "/client/.nvm" {
		t.Fatalf("bound spawn env PATH %q NVM_DIR %q", bound["PATH"], bound["NVM_DIR"])
	}
	bindSessionClient(t, nil, true)
	if !slices.Contains(childPathParts(mustSpawnEnv(t, env)), "/daemon/bin") {
		t.Fatal("unbound spawn env lost the daemon PATH")
	}
}
