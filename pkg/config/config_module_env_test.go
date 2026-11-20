package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Home named .icebath under a temp dir holding the given .env text
func dotenvHome(t *testing.T, content string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	InvalidateEnvCache()
	t.Cleanup(InvalidateEnvCache)
	return h
}

func TestDeprecatedCwdWarning(t *testing.T) {
	warn := func(h string) string {
		var errOut bytes.Buffer
		WarnDeprecatedCwdEnvVars(&errOut, h)
		return errOut.String()
	}
	t.Run("test_process_environment_does_not_trigger_warning", func(t *testing.T) {
		h := dotenvHome(t, "# TERMINAL_CWD=.\n")
		t.Setenv("MESSAGING_CWD", "/process/message-path")
		t.Setenv("TERMINAL_CWD", "/process/terminal-path")
		if got := warn(h); got != "" {
			t.Fatalf("stderr %q", got)
		}
	})
	t.Run("test_both_deprecated_vars_in_dotenv_warn", func(t *testing.T) {
		h := dotenvHome(t, "MESSAGING_CWD=/msg/path\nTERMINAL_CWD=/term/path\n")
		isolateProcessEnv(t, "MESSAGING_CWD", "TERMINAL_CWD")
		got := warn(h)
		for _, want := range []string{"MESSAGING_CWD", "TERMINAL_CWD", "config.yaml"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stderr lacks %s: %q", want, got)
			}
		}
		if !strings.Contains(strings.ToLower(got), "deprecated") {
			t.Fatalf("stderr %q", got)
		}
	})
	t.Run("test_dotenv_terminal_cwd_warns_with_explicit_config", func(t *testing.T) {
		h := dotenvHome(t, "TERMINAL_CWD=/legacy/path\n")
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("terminal:\n  cwd: /current/path\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := warn(h); !strings.Contains(got, "TERMINAL_CWD=/legacy/path") {
			t.Fatalf("stderr %q", got)
		}
	})
	t.Run("test_commented_and_empty_dotenv_values_do_not_warn", func(t *testing.T) {
		h := dotenvHome(t, "# MESSAGING_CWD=/commented\nTERMINAL_CWD=\n")
		t.Setenv("TERMINAL_CWD", "/process/bridge")
		if got := warn(h); got != "" {
			t.Fatalf("stderr %q", got)
		}
	})
	t.Run("test_dotenv_read_failure_is_silent", func(t *testing.T) {
		h := filepath.Join(t.TempDir(), ".icebath")
		if err := os.MkdirAll(filepath.Join(h, ".env"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", h)
		InvalidateEnvCache()
		t.Cleanup(InvalidateEnvCache)
		if got := warn(h); got != "" {
			t.Fatalf("stderr %q", got)
		}
	})
}

func TestDeprecatedCwdWarningPrintsTheUpstreamBlock(t *testing.T) {
	h := dotenvHome(t, "TERMINAL_CWD= /term/path \n")
	var errOut bytes.Buffer
	WarnDeprecatedCwdEnvVars(&errOut, h)
	want := "\033[33m\u26A0 Deprecated .env settings detected:\033[0m\n" +
		"  \033[33m\u26A0\033[0m TERMINAL_CWD=/term/path found in .env \u2014 this is deprecated.\n" +
		"  \033[2mMove to config.yaml instead:  terminal:\\n    cwd: /your/project/path\033[0m\n" +
		"  \033[2mThen remove the old entries from " + home.Display(h) + "/.env\033[0m\n\n"
	if errOut.String() != want {
		t.Fatalf("stderr %q, want %q", errOut.String(), want)
	}
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func TestRepeatCallsAreMemoizedButDeletedHomeIsRecreated(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	ensure := func() {
		t.Helper()
		if err := EnsureIcebathHome(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	}
	ensure()
	if !isDir(filepath.Join(h, "sessions")) {
		t.Fatal("sessions not created")
	}
	if err := os.RemoveAll(filepath.Join(h, "sessions")); err != nil {
		t.Fatal(err)
	}
	ensure()
	if _, err := os.Lstat(filepath.Join(h, "sessions")); err == nil {
		t.Fatal("a memoized call rebuilt the skeleton")
	}
	if err := os.RemoveAll(h); err != nil {
		t.Fatal(err)
	}
	ensure()
	if !isDir(filepath.Join(h, "sessions")) {
		t.Fatal("deleted home not recreated")
	}
}

func TestDistinctHomePathsEachGetTheSkeleton(t *testing.T) {
	tmp := t.TempDir()
	first, second := filepath.Join(tmp, "a", ".icebath"), filepath.Join(tmp, "b", ".icebath")
	for _, h := range []string{first, second} {
		t.Setenv("ICEBATH_HOME", h)
		if err := EnsureIcebathHome(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	}
	if !isDir(filepath.Join(first, "logs")) || !isDir(filepath.Join(second, "logs")) {
		t.Fatal("a home lacks its skeleton")
	}
}

const (
	lifecycleOldPAT = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	lifecycleNewPAT = "ghp_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestPlainLineSaveAndRemoveStillWork(t *testing.T) {
	h := dotenvHome(t, "")
	isolateProcessEnv(t, "GITHUB_TOKEN")
	if err := SaveEnvValueContext(t.Context(), h, "GITHUB_TOKEN", lifecycleOldPAT); err != nil {
		t.Fatal(err)
	}
	if got := LoadEnv(h)["GITHUB_TOKEN"]; got != lifecycleOldPAT {
		t.Fatalf("first save %q", got)
	}
	if err := SaveEnvValueContext(t.Context(), h, "GITHUB_TOKEN", lifecycleNewPAT); err != nil {
		t.Fatal(err)
	}
	if text := moduleRead(t, filepath.Join(h, ".env")); strings.Count(text, "GITHUB_TOKEN") != 1 {
		t.Fatalf(".env %q", text)
	}
	removed, err := RemoveHomeEnvValue(t.Context(), h, "GITHUB_TOKEN")
	if err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	if _, has := LoadEnv(h)["GITHUB_TOKEN"]; has {
		t.Fatal("token still loads")
	}
}

func TestWriterMatchesExactlyWhatLoadEnvAccepts(t *testing.T) {
	h := dotenvHome(t, "")
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"GITHUB_TOKEN=" + lifecycleOldPAT, true},
		{"export GITHUB_TOKEN=" + lifecycleOldPAT, true},
		{"GITHUB_TOKEN = " + lifecycleOldPAT, true},
		{"GITHUB_TOKEN\t=\t" + lifecycleOldPAT, true},
		{"export GITHUB_TOKEN = " + lifecycleOldPAT, true},
		{"# GITHUB_TOKEN=" + lifecycleOldPAT, false},
		{"GITHUB_TOKEN_EXTRA=" + lifecycleOldPAT, false},
		{"MY_GITHUB_TOKEN=" + lifecycleOldPAT, false},
	} {
		if err := os.WriteFile(filepath.Join(h, ".env"), []byte(tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		InvalidateEnvCache()
		if got := envLineDefines(tc.line, "GITHUB_TOKEN"); got != tc.want {
			t.Fatalf("writer says %v for %q", got, tc.line)
		}
		if _, loaded := LoadEnv(h)["GITHUB_TOKEN"]; loaded != tc.want {
			t.Fatalf("writer and load_env disagree on %q", tc.line)
		}
	}
}

func TestConfigLoadEnvStripsExportPrefix(t *testing.T) {
	h := dotenvHome(t, "export OPENAI_API_KEY=sk-export-123\nexport OPENROUTER_API_KEY=\"sk-or-456\"\nANTHROPIC_API_KEY=sk-plain-789\n")
	env := LoadEnv(h)
	if env["OPENAI_API_KEY"] != "sk-export-123" || env["OPENROUTER_API_KEY"] != "sk-or-456" || env["ANTHROPIC_API_KEY"] != "sk-plain-789" {
		t.Fatalf("env %q", env)
	}
	if _, has := env["export OPENAI_API_KEY"]; has {
		t.Fatal("the export prefix is part of a name")
	}
}
