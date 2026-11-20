package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func saveEnvHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestSaveEnvValuePreservesFileModeAndQuotesHash(t *testing.T) {
	h := saveEnvHome(t)
	path := filepath.Join(h, ".env")
	if err := os.WriteFile(path, []byte("EXISTING=value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o640)
	if err := SaveEnvValue(h, "TENOR_API_KEY", "sk-test-secret"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	token := "sk-ant-oat01-abc#xyz#more"
	t.Setenv("ANTHROPIC_TOKEN", "")
	if err := SaveEnvValue(h, "ANTHROPIC_TOKEN", token); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `ANTHROPIC_TOKEN="`+token+`"`) || LoadEnvFile(path)["ANTHROPIC_TOKEN"] != token {
		t.Fatalf("env %q", raw)
	}
}

func TestSaveEnvValueQuotedInputIsStable(t *testing.T) {
	h := saveEnvHome(t)
	path := filepath.Join(h, ".env")
	value := `"/Users/me/Application Support/key"`
	t.Setenv("TERMINAL_SSH_KEY", "")
	SaveEnvValue(h, "TERMINAL_SSH_KEY", value)
	first, _ := os.ReadFile(path)
	SaveEnvValue(h, "TERMINAL_SSH_KEY", value)
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("%q then %q", first, second)
	}
	var line string
	for _, l := range strings.Split(string(first), "\n") {
		if strings.HasPrefix(l, "TERMINAL_SSH_KEY=") {
			line = l
		}
	}
	if !strings.HasPrefix(line, `TERMINAL_SSH_KEY="`) || !strings.HasSuffix(line, `"`) || strings.Count(line, `TERMINAL_SSH_KEY="`) != 1 ||
		LoadEnvFile(path)["TERMINAL_SSH_KEY"] != value {
		t.Fatalf("line %q loaded %q", line, LoadEnvFile(path)["TERMINAL_SSH_KEY"])
	}
}

func TestSaveEnvValueWriterDenylist(t *testing.T) {
	h := saveEnvHome(t)
	path := filepath.Join(h, ".env")
	for _, key := range []string{"ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_SPOTIFY_CLIENT_ID", "ICEBATH_QWEN_BASE_URL", "ICEBATH_MAX_ITERATIONS",
		"GIT_COMMITTER_NAME", "GIT_AUTHOR_NAME", "GIT_TERMINAL_PROMPT", "GIT_EDITOR_WIDE", "git_config_parameters", "ld_preload"} {
		t.Setenv(key, "")
		if err := SaveEnvValue(h, key, "test-value-123"); err != nil || LoadEnvFile(path)[key] != "test-value-123" {
			t.Fatalf("%s refused %v", key, err)
		}
	}
	for _, key := range []string{"ICEBATH_CONFIG_PATH", "ICEBATH_ENV_PATH", "ICEBATH_OPTIONAL_MCPS", "ICEBATH_YOLO_MODE", "ICEBATH_ACCEPT_HOOKS",
		"ICEBATH_REDACT_SECRETS", "ICEBATH_INTERACTIVE", "ICEBATH_EXEC_ASK", "ICEBATH_GATEWAY_SESSION", "ICEBATH_CRON_SESSION",
		"ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_KEY", "ICEBATH_SESSION_PLATFORM",
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_KEY_17",
		"GIT_CONFIG_VALUE_17", "GIT_SSH", "GIT_ASKPASS", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_EXTERNAL_DIFF",
		"GIT_PROXY_COMMAND", "GIT_TEMPLATE_DIR", "GIT_DIR", "SSH_ASKPASS", "SUDO_ASKPASS", "LD_PROFILE", "DYLD_PRINT_LIBRARIES",
		"BASH_ENV", "ENV", "ZDOTDIR", "PROMPT_COMMAND", "VIMINIT", "EXINIT", "MANPAGER", "PERL5OPT", "PERL5LIB", "PERLLIB", "RUBYOPT",
		"RUBYLIB", "PYTHONBREAKPOINT", "PYTHONCASEOK", "CLASSPATH", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS", "GOFLAGS", "RUSTFLAGS"} {
		if err := SaveEnvValue(h, key, "1"); err == nil || !strings.Contains(err.Error(), "denylist") {
			t.Fatalf("%s written %v", key, err)
		}
		if _, ok := LoadEnvFile(path)[key]; ok {
			t.Fatalf("%s persisted", key)
		}
	}
	catalog := filepath.Join(h, "custom-mcp-catalog")
	os.WriteFile(path, []byte("ICEBATH_OPTIONAL_MCPS="+catalog+"\n"), 0o600)
	if LoadEnvFile(path)["ICEBATH_OPTIONAL_MCPS"] != catalog {
		t.Fatal("preexisting override no longer loads")
	}
}

func TestSaveEnvValueWindowsPolicyNames(t *testing.T) {
	h := saveEnvHome(t)
	prev := envPolicyWindows
	t.Cleanup(func() { envPolicyWindows = prev })
	envPolicyWindows = false
	if envVarPolicyName("Path") != "Path" || envLineDefines("Path=C:\\Windows\\System32\n", "PATH") || envLineDefines("export Path=C:\\Windows\n", "PATH") {
		t.Fatal("posix policy folded case")
	}
	envPolicyWindows = true
	for key, want := range map[string]string{"Path": "PATH", "Icebath_Yolo_Mode": "ICEBATH_YOLO_MODE"} {
		if got := envVarPolicyName(key); got != want {
			t.Fatalf("%s -> %s", key, got)
		}
	}
	if !envLineDefines("Path=C:\\Windows\\System32\n", "PATH") || !envLineDefines("export Path=C:\\Windows\\System32\n", "PATH") {
		t.Fatal("windows policy kept case")
	}
	for _, key := range []string{"Ld_Preload", "Git_Config_Parameters", "Icebath_Yolo_Mode", "Icebath_Optional_Mcps"} {
		if err := SaveEnvValue(h, key, "1"); err == nil || !strings.Contains(err.Error(), "denylist") {
			t.Fatalf("%s written %v", key, err)
		}
	}
}

func TestSaveEnvValueExportAndSpacedLinesAreReplaced(t *testing.T) {
	h := saveEnvHome(t)
	path := filepath.Join(h, ".env")
	for _, seed := range []string{"export GITHUB_TOKEN=ghp_old\n", "GITHUB_TOKEN = ghp_old\n", ""} {
		os.WriteFile(path, []byte(seed), 0o600)
		t.Setenv("GITHUB_TOKEN", "")
		if err := SaveEnvValue(h, "GITHUB_TOKEN", "ghp_new"); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "ghp_old") || strings.Count(string(raw), "GITHUB_TOKEN") != 1 || LoadEnvFile(path)["GITHUB_TOKEN"] != "ghp_new" {
			t.Fatalf("seed %q env %q", seed, raw)
		}
		if removed, err := RemoveEnvValue(path, "GITHUB_TOKEN"); !removed || err != nil {
			t.Fatalf("remove %v %v", removed, err)
		}
		if _, ok := LoadEnvFile(path)["GITHUB_TOKEN"]; ok {
			t.Fatal("token resurrected after remove")
		}
	}
	os.WriteFile(path, []byte("# export GITHUB_TOKEN=ghp_old\nOTHER_KEY=value\n"), 0o600)
	if removed, _ := RemoveEnvValue(path, "GITHUB_TOKEN"); removed {
		t.Fatal("commented line treated as an assignment")
	}
}

func TestSaveEnvValueRefusesLaunchEnvHook(t *testing.T) {
	h := saveEnvHome(t)
	prevHook, prevWin := EnvWriterLaunchDenylist, envPolicyWindows
	t.Cleanup(func() { EnvWriterLaunchDenylist, envPolicyWindows = prevHook, prevWin })
	EnvWriterLaunchDenylist = func() map[string]bool {
		return map[string]bool{"ICEBATH_COPILOT_ACP_COMMAND": true, "ICEBATH_COPILOT_ACP_ARGS": true, "COPILOT_CLI_PATH": true}
	}
	for _, key := range []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH"} {
		if err := SaveEnvValue(h, key, "1"); err == nil || !strings.Contains(err.Error(), "denylist") {
			t.Fatalf("%s written %v", key, err)
		}
	}
	envPolicyWindows = true
	for _, key := range []string{"Icebath_Copilot_Acp_Command", "Icebath_Copilot_Acp_Args"} {
		if err := SaveEnvValue(h, key, "1"); err == nil || !strings.Contains(err.Error(), "denylist") {
			t.Fatalf("%s written %v", key, err)
		}
	}
}
