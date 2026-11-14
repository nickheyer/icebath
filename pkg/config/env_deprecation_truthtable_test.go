package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeprecatedEnvWarningTruthTable(t *testing.T) {
	const realPath = "/home/user/projects/demo"
	str := func(s string) *string { return &s }
	rows := []struct {
		id         string
		envFile    *string
		procEnv    map[string]string
		configYAML *string
		warnKeys   []string
	}{
		{"all-absent-silent", nil, nil, nil, nil},
		{"no-envfile-procenv-only-silent", nil, map[string]string{"TERMINAL_CWD": realPath}, nil, nil},
		{"procenv-dot-only-silent-88829", str("API_KEY=x\n"), map[string]string{"TERMINAL_CWD": "."}, nil, nil},
		{"procenv-realpath-only-silent-89016", str("API_KEY=x\n"), map[string]string{"TERMINAL_CWD": realPath}, nil, nil},
		{"config-bridged-procenv-silent-89389", str("API_KEY=x\n"), map[string]string{"TERMINAL_CWD": realPath}, str("terminal:\n  cwd: " + realPath + "\n"), nil},
		{"config-only-silent-90299", str("API_KEY=x\n"), nil, str("terminal:\n  cwd: " + realPath + "\n"), nil},
		{"dotenv-dot-warns", str("TERMINAL_CWD=.\n"), nil, nil, []string{"TERMINAL_CWD"}},
		{"dotenv-realpath-warns", str("TERMINAL_CWD=" + realPath + "\n"), nil, nil, []string{"TERMINAL_CWD"}},
		{"dotenv-dot-warns-despite-config", str("TERMINAL_CWD=.\n"), nil, str("terminal:\n  cwd: /other/path\n"), []string{"TERMINAL_CWD"}},
		{"dotenv-warns-despite-procenv-and-config", str("TERMINAL_CWD=" + realPath + "\n"), map[string]string{"TERMINAL_CWD": realPath}, str("terminal:\n  cwd: " + realPath + "\n"), []string{"TERMINAL_CWD"}},
		{"dotenv-empty-value-silent", str("TERMINAL_CWD=\n"), map[string]string{"TERMINAL_CWD": realPath}, nil, nil},
		{"dotenv-commented-silent", str("# TERMINAL_CWD=.\n"), map[string]string{"TERMINAL_CWD": "."}, nil, nil},
		{"dotenv-export-prefix-warns", str("export TERMINAL_CWD=" + realPath + "\n"), nil, nil, []string{"TERMINAL_CWD"}},
		{"quiet-mode-no-gating-dotenv-warns", str("TERMINAL_CWD=.\n"), map[string]string{"ICEBATH_QUIET": "1"}, nil, []string{"TERMINAL_CWD"}},
		{"quiet-mode-procenv-only-silent", str("API_KEY=x\n"), map[string]string{"ICEBATH_QUIET": "1", "TERMINAL_CWD": "."}, nil, nil},
		{"dotenv-messaging-cwd-warns", str("MESSAGING_CWD=/msg/path\n"), nil, nil, []string{"MESSAGING_CWD"}},
		{"procenv-messaging-cwd-silent", str("API_KEY=x\n"), map[string]string{"MESSAGING_CWD": "/msg/path"}, nil, nil},
		{"dotenv-both-keys-warn", str("MESSAGING_CWD=/msg/path\nTERMINAL_CWD=" + realPath + "\n"), nil, nil, []string{"MESSAGING_CWD", "TERMINAL_CWD"}},
	}
	for _, row := range rows {
		t.Run(row.id, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), ".icebath")
			if err := os.Mkdir(h, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			if row.envFile != nil {
				if err := os.WriteFile(filepath.Join(h, ".env"), []byte(*row.envFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if row.configYAML != nil {
				if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(*row.configYAML), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"TERMINAL_CWD", "MESSAGING_CWD", "ICEBATH_QUIET"} {
				t.Setenv(key, "")
				os.Unsetenv(key)
			}
			for k, v := range row.procEnv {
				t.Setenv(k, v)
			}
			InvalidateEnvCache()
			var errOut bytes.Buffer
			WarnDeprecatedCwdEnvVars(&errOut, h)
			got := errOut.String()
			if len(row.warnKeys) == 0 {
				if got != "" {
					t.Fatalf("expected silence, got:\n%s", got)
				}
				return
			}
			if strings.TrimSpace(got) == "" {
				t.Fatal("expected a deprecation warning on stderr")
			}
			for _, key := range row.warnKeys {
				if !strings.Contains(got, key) {
					t.Fatalf("expected %s in warning:\n%s", key, got)
				}
			}
		})
	}
}
