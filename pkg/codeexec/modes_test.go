package codeexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type probeOutcome struct {
	result probeResult
	ok     bool
}

// Scripted interpreter probes in order, counting the calls made
func scriptProbes(t *testing.T, outcomes ...probeOutcome) *int {
	t.Helper()
	calls := 0
	prev := probePython
	probePython = func(*tools.Env, string, string) (probeResult, bool) {
		if calls >= len(outcomes) {
			t.Fatal("unexpected probe")
		}
		calls++
		o := outcomes[min(calls, len(outcomes))-1]
		return o.result, o.ok
	}
	t.Cleanup(func() { probePython = prev })
	return &calls
}

func TestProbeRetriesTransientFailureAndCachesSuccess(t *testing.T) {
	env := testEnv(t)
	transient := probeOutcome{}
	recovered := probeOutcome{probeResult{0, ""}, true}
	usablePythonCache.clear()
	t.Cleanup(usablePythonCache.clear)
	calls := scriptProbes(t, transient, recovered)
	if isUsablePython(env, "probe-python") || !isUsablePython(env, "probe-python") || !isUsablePython(env, "probe-python") || *calls != 2 {
		t.Fatalf("usable probe calls %d", *calls)
	}
}

func TestInterpreterComesFromTheSessionPathAndOldOnesAreSkipped(t *testing.T) {
	if isWindows {
		t.Skip("the fixture interpreters are shell scripts")
	}
	env := testEnv(t)
	usablePythonCache.clear()
	t.Cleanup(usablePythonCache.clear)
	old, current, empty := t.TempDir(), t.TempDir(), t.TempDir()
	for dir, status := range map[string]string{old: "1", current: "0"} {
		if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nexit "+status+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(empty, "python"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{empty, old, current}, string(os.PathListSeparator)))
	for _, mode := range []string{"strict", "project"} {
		unsetEnv(t, "VIRTUAL_ENV")
		unsetEnv(t, "CONDA_PREFIX")
		if got, err := resolveChildPython(t.Context(), env, mode); err != nil || got != filepath.Join(current, "python3") {
			t.Fatalf("%s mode ran %q err %v", mode, got, err)
		}
	}
	if got, err := RuntimePython(t.Context()); err != nil || got != filepath.Join(current, "python3") {
		t.Fatalf("doctor sees %q err %v", got, err)
	}
	t.Setenv("PATH", empty)
	if _, err := resolveChildPython(t.Context(), env, "strict"); err == nil || !strings.Contains(err.Error(), "no Python 3.8 or newer interpreter on PATH") {
		t.Fatalf("a PATH without an interpreter resolved: %v", err)
	}
}

func TestProjectWithoutActiveVenvFallsBack(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	unsetEnv(t, "VIRTUAL_ENV")
	unsetEnv(t, "CONDA_PREFIX")
	if got, err := resolveChildPython(t.Context(), env, "project"); err != nil || got != rt.Python {
		t.Fatalf("child python %q err %v", got, err)
	}
}

func TestProjectStaleCwdFallsThroughThenUsesProcessCwd(t *testing.T) {
	env := testEnv(t)
	dir := t.TempDir()
	if err := tools.RegisterTaskEnvOverrides(env, "stale", map[string]any{"cwd": dir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tools.ClearTaskEnvOverrides(env, "stale") })
	tools.RecordSessionCwd(env, "stale", filepath.Join(dir, "deleted"))
	t.Setenv("TERMINAL_CWD", filepath.Join(dir, "missing"))
	if got, err := resolveChildCwd(env, "project", "staging", "stale"); err != nil || got != dir {
		t.Fatalf("stale task cwd %q err %v", got, err)
	}
	wd, _ := os.Getwd()
	if got, err := resolveChildCwd(env, "project", "staging", ""); err != nil || got != wd {
		t.Fatalf("fallback cwd %q err %v", got, err)
	}
}

func TestModeSchemaMatchesConfigWithoutClaimingIsolation(t *testing.T) {
	for mode, description := range map[string]string{"strict": "temp dir", "project": "session"} {
		env := testEnv(t)
		writeKernelConfig(t, env, map[string]any{"mode": mode})
		text := strings.ToLower(buildExecuteCodeSchema(env, nil, "")["description"].(string))
		if !strings.Contains(text, description) {
			t.Fatalf("%s schema lacks %q", mode, description)
		}
		for _, word := range []string{"sandbox", "isolated", "cloud"} {
			if strings.Contains(text, word) {
				t.Fatalf("%s schema claims %q", mode, word)
			}
		}
	}
}

func TestCredentialPolicyAndWhitelistInRealChild(t *testing.T) {
	blocked := []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "MY_SECRET", "DB_PASSWORD", "VAULT_CREDENTIAL",
		"LDAP_PASSWD", "AUTH_TOKEN", "SENTRY_DSN", "SLACK_WEBHOOK", "HOME_APIKEY", "USER_CREDS", "TERM_BEARER",
		"ICEBATH_BASE_URL", "ICEBATH_INTERACTIVE", "BUZZ_PRIVATE_KEY", "RANDOM_UNKNOWN"}
	allowed := []string{"ICEBATH_PROFILE", "ICEBATH_CONFIG", "ICEBATH_ENV", "LC_ENV_TEST", "TENOR_API_KEY"}
	for _, mode := range []string{"strict", "project"} {
		t.Run(mode, func(t *testing.T) {
			bundled := tools.BundledPlatformSecretEnvs
			env := kernelEnv(t, map[string]any{"mode": mode})
			tools.BundledPlatformSecretEnvs = bundled
			unsetEnv(t, "VIRTUAL_ENV")
			unsetEnv(t, "CONDA_PREFIX")
			names := append(append([]string{}, blocked...), allowed...)
			for _, name := range names {
				t.Setenv(name, "fake-"+name)
			}
			tools.RegisterEnvPassthrough(env, []string{"TENOR_API_KEY", "OPENAI_API_KEY", "BUZZ_PRIVATE_KEY"})
			quoted := make([]string, len(names))
			for i, n := range names {
				quoted[i] = "'" + n + "'"
			}
			code := "import json, os, icebath_tools\n" +
				"print(json.dumps({'env': {k: os.environ.get(k) for k in (" + strings.Join(quoted, ", ") + ")},\n" +
				"                  'recursive': hasattr(icebath_tools, 'execute_code'),\n" +
				"                  'delegation': hasattr(icebath_tools, 'delegate_task')}))\n"
			result := decodeResult(t, executeCode(t.Context(), env, code, "child-env-test", []string{"read_file", "execute_code", "delegate_task"}, true))
			wantStatus(t, result, "success")
			var probe struct {
				Env        map[string]*string `json:"env"`
				Recursive  bool               `json:"recursive"`
				Delegation bool               `json:"delegation"`
			}
			if err := json.Unmarshal([]byte(outputOf(result)), &probe); err != nil {
				t.Fatalf("probe output %v: %v", result, err)
			}
			for _, name := range blocked {
				if probe.Env[name] != nil {
					t.Fatalf("%s reached the child as %q", name, *probe.Env[name])
				}
			}
			for _, name := range allowed {
				if probe.Env[name] == nil || *probe.Env[name] != "fake-"+name {
					t.Fatalf("%s did not reach the child: %v", name, probe.Env[name])
				}
			}
			if probe.Recursive || probe.Delegation {
				t.Fatalf("stubs exposed recursion %v delegation %v", probe.Recursive, probe.Delegation)
			}
		})
	}
}
