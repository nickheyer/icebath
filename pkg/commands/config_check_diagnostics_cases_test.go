package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home at the latest config version with body appended and an optional dotenv
func checkHome(t *testing.T, h, body, env string) string {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("_config_version: %d\n%s", config.LatestConfigVersion(), body)
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

// Config check and the migration warning agree on which saved platform toolsets are unknown
func TestConfigCheckAndMigrateAgreeOnStalePlatformToolsets(t *testing.T) {
	prev := tools.PluginToolsetCatalog
	t.Cleanup(func() { tools.PluginToolsetCatalog = prev })
	tools.PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { return map[string]bool{} }
	tools.PluginToolsetCatalog.PortableNowait = func(context.Context) map[string]bool { return map[string]bool{"portable": true} }
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	root := t.TempDir()
	stale := checkHome(t, filepath.Join(root, "stale"), "platform_toolsets:\n"+
		"  cli: [hermes-cli, messaging, linear, mcp-linear, mcp-portable, mcp-ghost, ghost]\n"+
		"  teams: [hermes-teams]\n"+
		"mcp_servers:\n"+
		"  linear:\n"+
		"    command: linear-mcp\n"+
		"    enabled: false\n"+
		"known_plugin_toolsets:\n"+
		"  cli: [ghost]\n", "")
	clean := checkHome(t, filepath.Join(root, "clean"), "platform_toolsets:\n  cli: [hermes-cli]\nmcp_servers: [a]\n", "")
	staleWant := map[string]bool{"messaging": true, "ghost": true, "mcp-ghost": true}
	for _, c := range []struct {
		h    string
		want map[string]bool
	}{{stale, staleWant}, {clean, map[string]bool{}}, {stale, staleWant}} {
		output := runConfigCheck(t, c.h, ConfigCommandSources(c.h))
		for _, name := range []string{"messaging", "ghost", "mcp-ghost", "linear", "mcp-linear", "mcp-portable", "hermes-teams"} {
			if got := strings.Contains(output, "unknown toolset '"+name+"'"); got != c.want[name] {
				t.Fatalf("%s %s reported %v\n%s", filepath.Base(c.h), name, got, output)
			}
		}
		results, err := config.MigrateConfig(context.Background(), c.h, false, true)
		if err != nil {
			t.Fatal(err)
		}
		var warned, printed []string
		for _, w := range results.Warnings {
			if strings.Contains(w, "unknown toolset") {
				warned = append(warned, w)
			}
		}
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "unknown toolset") {
				printed = append(printed, strings.TrimPrefix(strings.TrimSpace(line), "⚠ "))
			}
		}
		sort.Strings(warned)
		sort.Strings(printed)
		if !reflect.DeepEqual(dedupe(warned), dedupe(printed)) {
			t.Fatalf("%s migrate %q check %q", filepath.Base(c.h), warned, printed)
		}
	}
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
