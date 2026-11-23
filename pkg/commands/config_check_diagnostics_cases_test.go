package commands

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Home at the latest config version with body appended and an optional dotenv
func checkHome(t *testing.T, h, body, env string) string {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	text := body
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(h, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Stdout of config check run against the home with the given sources
func runConfigCheck(t *testing.T, h string, src config.ConfigCommandSources) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	config.InvalidateEnvCache()
	var out bytes.Buffer
	prevOut, prevNotice := config.FacadeStdout, config.FacadeNoticeOut
	config.FacadeStdout, config.FacadeNoticeOut = &out, io.Discard
	defer func() { config.FacadeStdout, config.FacadeNoticeOut = prevOut, prevNotice }()
	if err := config.ConfigCommand(context.Background(), h, config.ConfigArgs{Command: "check"}, src); err != nil {
		t.Fatalf("config check: %v", err)
	}
	return out.String()
}

func dedupe(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// A disabled platform plugin is reported only when the runtime disables it and its credential is configured
func TestConfigCheckReportsDisabledPlatformOnlyWhenRuntimeDisablesIt(t *testing.T) {
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	t.Setenv("FAKECHAT_TOKEN", "")
	os.Unsetenv("FAKECHAT_TOKEN")
	root := t.TempDir()
	token := "FAKECHAT_TOKEN=synthetic-test-token\n"
	cases := []struct {
		h        string
		reported bool
	}{
		{checkHome(t, filepath.Join(root, "key"), "plugins:\n  disabled: [platforms/fakechat]\n", token), true},
		{checkHome(t, filepath.Join(root, "name"), "plugins:\n  disabled: [fakechat-platform]\n", token), true},
		{checkHome(t, filepath.Join(root, "no_token"), "plugins:\n  disabled: [platforms/fakechat]\n", ""), false},
		{checkHome(t, filepath.Join(root, "string"), "plugins:\n  disabled: platforms/fakechat\n", token), false},
	}
	for _, c := range append(cases, cases[0]) {
		src := ConfigCommandSources(c.h)
		src.CheckDiagnostics.BundledPlatformManifests = func() ([]config.PlatformManifest, error) {
			return []config.PlatformManifest{{Name: "fakechat", ManifestName: "fakechat-platform", RequiredEnv: []string{"FAKECHAT_TOKEN"}}}, nil
		}
		output := runConfigCheck(t, c.h, src)
		if got := strings.Contains(output, "platform plugin 'platforms/fakechat' is disabled"); got != c.reported {
			t.Fatalf("%s reported %v\n%s", filepath.Base(c.h), got, output)
		}
		if c.reported && !strings.Contains(output, "icebath plugins enable platforms/fakechat") {
			t.Fatalf("%s no enable hint\n%s", filepath.Base(c.h), output)
		}
		if strings.Contains(output, "synthetic-test-token") {
			t.Fatalf("%s leaked the token", filepath.Base(c.h))
		}
	}
}
