package provider

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRuntimeParseArgs(t *testing.T) {
	for raw, want := range map[string]string{"on": "codex_app_server", "OFF": "auto", "codex": "codex_app_server", "icebath": "auto", " auto ": "auto", "codex_app_server": "codex_app_server"} {
		if got, errs := CodexRuntimeParseArgs(raw); got == nil || *got != want || errs != nil {
			t.Fatalf("%q: %v %v", raw, got, errs)
		}
	}
	if got, errs := CodexRuntimeParseArgs(""); got != nil || errs != nil {
		t.Fatalf("empty %v %v", got, errs)
	}
	if got, errs := CodexRuntimeParseArgs("Bogus"); got != nil || len(errs) != 1 || errs[0] != "Unknown runtime 'bogus'. Use one of: auto, codex_app_server, on, off" {
		t.Fatalf("unknown %v %v", got, errs)
	}
}

func TestCodexRuntimeConfigReadsAndWrites(t *testing.T) {
	for _, cfg := range []map[string]any{nil, {}, {"model": "x"}, {"model": map[string]any{"openai_runtime": "weird"}}, {"model": map[string]any{"openai_runtime": nil}}} {
		if got := GetCurrentCodexRuntime(cfg); got != "auto" {
			t.Fatalf("%v: %q", cfg, got)
		}
	}
	if got := GetCurrentCodexRuntime(map[string]any{"model": map[string]any{"openai_runtime": " Codex_App_Server "}}); got != "codex_app_server" {
		t.Fatalf("got %q", got)
	}
	if GetConfiguredCodexBinary(nil) != "codex" || GetConfiguredCodexBinary(map[string]any{"model": map[string]any{"codex_bin": "  "}}) != "codex" {
		t.Fatal("default binary")
	}
	if got := GetConfiguredCodexBinary(map[string]any{"model": map[string]any{"codex_bin": " /opt/Codex.app/codex "}}); got != "/opt/Codex.app/codex" {
		t.Fatalf("binary %q", got)
	}
	cfg := map[string]any{"model": "not a dict"}
	if old, err := SetCodexRuntime(cfg, "codex_app_server"); err != nil || old != "auto" || cfg["model"].(map[string]any)["openai_runtime"] != "codex_app_server" {
		t.Fatalf("set %v %v %v", old, err, cfg)
	}
	if _, err := SetCodexRuntime(cfg, "bogus"); err == nil || err.Error() != "invalid runtime 'bogus'; must be one of ('auto', 'codex_app_server')" {
		t.Fatalf("invalid %v", err)
	}
}

func TestApplyCodexRuntimeStatesAndRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-codex")
	cfg := map[string]any{"model": map[string]any{"codex_bin": missing}}
	status, err := ApplyCodexRuntime(context.Background(), cfg, nil, nil)
	if err != nil || !status.Success || *status.NewValue != "auto" || !strings.HasPrefix(status.Message, "openai_runtime: auto\ncodex CLI: not available — ") {
		t.Fatalf("show %+v %v", status, err)
	}
	auto := "auto"
	if status, _ := ApplyCodexRuntime(context.Background(), cfg, &auto, nil); !status.Success || status.Message != "openai_runtime already set to auto" || status.RequiresNewSession {
		t.Fatalf("already %+v", status)
	}
	enable := "codex_app_server"
	status, _ = ApplyCodexRuntime(context.Background(), cfg, &enable, func(map[string]any) error { t.Fatal("persisted a refused enable"); return nil })
	if status.Success || status.NewValue != nil || !strings.HasPrefix(status.Message, "Cannot enable codex_app_server runtime: ") || !strings.HasSuffix(status.Message, "\nInstall with: npm i -g @openai/codex") {
		t.Fatalf("refused %+v", status)
	}
	if GetCurrentCodexRuntime(cfg) != "auto" {
		t.Fatal("refused enable changed the config")
	}
	on := map[string]any{"model": map[string]any{"openai_runtime": "codex_app_server", "codex_bin": missing}}
	var persisted int
	status, _ = ApplyCodexRuntime(context.Background(), on, &auto, func(map[string]any) error { persisted++; return nil })
	if !status.Success || !status.RequiresNewSession || persisted != 1 || status.Message != "openai_runtime: codex_app_server → auto\nOpenAI/Codex turns will use the default Icebath runtime.\nEffective on next session." {
		t.Fatalf("disable %+v %d", status, persisted)
	}
	on["model"].(map[string]any)["openai_runtime"] = "codex_app_server"
	status, _ = ApplyCodexRuntime(context.Background(), on, &auto, func(map[string]any) error { return errors.New("disk full") })
	if status.Success || *status.NewValue != "auto" || status.Message != "updated config in memory but persist failed: disk full" {
		t.Fatalf("persist failure %+v", status)
	}
	if _, err := ApplyCodexRuntime(context.Background(), map[string]any{}, new("bogus"), nil); err == nil {
		t.Fatal("invalid runtime accepted")
	}
}

// Swaps the switch's binary probe and migration for one test
func withCodexSwitchFakes(t *testing.T, check func(string) (bool, string), migrate func(context.Context, map[string]any, CodexMigrateOptions) (*CodexMigrationReport, error)) {
	t.Helper()
	prevCheck, prevMigrate := codexRuntimeCheckBinary, codexRuntimeMigrate
	codexRuntimeCheckBinary, codexRuntimeMigrate = check, migrate
	t.Cleanup(func() { codexRuntimeCheckBinary, codexRuntimeMigrate = prevCheck, prevMigrate })
}

func fakeCodexMigration(calls *int) func(context.Context, map[string]any, CodexMigrateOptions) (*CodexMigrationReport, error) {
	return func(context.Context, map[string]any, CodexMigrateOptions) (*CodexMigrationReport, error) {
		*calls++
		return &CodexMigrationReport{Migrated: []string{"filesystem", CodexToolsMCPServerName}, WrotePermissionsDefault: ":workspace", TargetPath: "/fake/.codex/config.toml"}, nil
	}
}

func TestApplyCodexRuntimeBinaryCheckUsesConfiguredPath(t *testing.T) {
	configured := "/Applications/Codex.app/Contents/Resources/codex"
	var probed []string
	withCodexSwitchFakes(t, func(bin string) (bool, string) { probed = append(probed, bin); return true, "0.130.0" }, nil)
	status, err := ApplyCodexRuntime(context.Background(), map[string]any{"model": map[string]any{"openai_runtime": "codex_app_server", "codex_bin": configured}}, nil, nil)
	if err != nil || !status.Success || len(probed) != 1 || probed[0] != configured {
		t.Fatalf("%+v %v %v", status, err, probed)
	}
}

func TestApplyCodexRuntimeReapplyAndEnableRunTheMigration(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"model": map[string]any{"openai_runtime": "codex_app_server"}, "mcp_servers": map[string]any{"filesystem": map[string]any{"command": "npx", "args": []any{"-y", "fs-server"}}}},
		{"mcp_servers": map[string]any{"filesystem": map[string]any{"command": "npx", "args": []any{"-y", "fs-server"}}}},
	} {
		reapply := GetCurrentCodexRuntime(cfg) == "codex_app_server"
		calls, persisted := 0, 0
		withCodexSwitchFakes(t, func(string) (bool, string) { return true, "0.130.0" }, fakeCodexMigration(&calls))
		status, err := ApplyCodexRuntime(context.Background(), cfg, new("codex_app_server"), func(map[string]any) error { persisted++; return nil })
		if err != nil || !status.Success || calls != 1 || !status.RequiresNewSession {
			t.Fatalf("%+v %v calls %d", status, err, calls)
		}
		for _, want := range []string{"Migrated 1 MCP server", "filesystem", "Default sandbox: :workspace", "codex CLI: 0.130.0", codexToolsCallbackNote[0], "  (config: /fake/.codex/config.toml)"} {
			if !strings.Contains(status.Message, want) {
				t.Fatalf("message lacks %q: %s", want, status.Message)
			}
		}
		if reapply && (persisted != 0 || !strings.HasPrefix(status.Message, "openai_runtime already set to codex_app_server — re-applying migration")) {
			t.Fatalf("reapply persisted %d: %s", persisted, status.Message)
		}
		if !reapply && (persisted != 1 || !strings.HasPrefix(status.Message, "openai_runtime: auto → codex_app_server")) {
			t.Fatalf("enable persisted %d: %s", persisted, status.Message)
		}
	}
}

func TestApplyCodexRuntimeDisableDoesNotTriggerMigration(t *testing.T) {
	calls := 0
	withCodexSwitchFakes(t, func(string) (bool, string) { return true, "0.130.0" }, fakeCodexMigration(&calls))
	status, err := ApplyCodexRuntime(context.Background(), map[string]any{"model": map[string]any{"openai_runtime": "codex_app_server"}, "mcp_servers": map[string]any{"x": map[string]any{"command": "y"}}}, new("auto"), nil)
	if err != nil || !status.Success || calls != 0 {
		t.Fatalf("%+v %v calls %d", status, err, calls)
	}
}

func TestApplyCodexRuntimeMigrationFailureDoesNotBlockEnable(t *testing.T) {
	withCodexSwitchFakes(t, func(string) (bool, string) { return true, "0.130.0" }, func(context.Context, map[string]any, CodexMigrateOptions) (*CodexMigrationReport, error) {
		return nil, errors.New("disk full")
	})
	status, err := ApplyCodexRuntime(context.Background(), map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": "y"}}}, new("codex_app_server"), nil)
	if err != nil || !status.Success || *status.NewValue != "codex_app_server" || !strings.Contains(status.Message, "⚠ MCP migration skipped: disk full") {
		t.Fatalf("%+v %v", status, err)
	}
}
