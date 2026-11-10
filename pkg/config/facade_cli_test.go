package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Captures facade stdout and stderr for one test
func captureFacade(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	prevOut, prevErr, prevRefusal := FacadeStdout, FacadeNoticeOut, EnvRefusalOut
	FacadeStdout, FacadeNoticeOut, EnvRefusalOut = out, errOut, errOut
	t.Cleanup(func() { FacadeStdout, FacadeNoticeOut, EnvRefusalOut = prevOut, prevErr, prevRefusal })
	return out, errOut
}

// Command collaborators with upper snake env setting routing and no stale route to drop
func testCommandSources() ConfigCommandSources {
	return ConfigCommandSources{
		LeftCoreTerminalKeys: map[string]string{"terminal.singularity_image": "TERMINAL_SINGULARITY_IMAGE"},
		IsEnvSettingKey: func(key string) bool {
			return !strings.Contains(key, ".") && key == strings.ToUpper(key) && !IsEnvConfigKey(key)
		},
		SaveEnvSetting: func(_ context.Context, h, key, value string) error {
			return SaveEnvValue(h, strings.ToUpper(key), value)
		},
		ReadEnvSetting: func(h, key string) (string, bool) { v, ok := LoadEnv(h)[strings.ToUpper(key)]; return v, ok },
		RemoveEnvSetting: func(_ context.Context, h, key string) (bool, error) {
			return RemoveHomeEnvValue(context.Background(), h, key)
		},
		DropStaleModelRoute: func(map[string]any, string, map[string]any) (map[string]any, bool) { return nil, false },
	}
}

func readSaved(t *testing.T, path string, keys ...string) (any, bool) {
	t.Helper()
	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	return CfgGet(doc, keys...)
}

func exitCode(err error) int {
	var exit *ConfigExit
	if errors.As(err, &exit) {
		return exit.Code
	}
	return -1
}

// Sets one value in a fresh facade home and returns the saved leaf
func setAndRead(t *testing.T, key, value string, path ...string) (any, bool) {
	t.Helper()
	h, cfgPath := facadeHome(t, "")
	captureFacade(t)
	if err := SetConfigValue(context.Background(), h, key, value, false, testCommandSources()); err != nil {
		t.Fatalf("%s=%q: %v", key, value, err)
	}
	return readSaved(t, cfgPath, path...)
}

func TestNumericCoercion(t *testing.T) {
	check := func(t *testing.T, key, value string, want any, path ...string) {
		t.Helper()
		got, ok := setAndRead(t, key, value, path...)
		if !ok || !pyEq(got, want) || PyTypeName(got) != PyTypeName(want) {
			t.Fatalf("%s=%q saved %#v want %#v", key, value, got, want)
		}
	}
	t.Run("test_negative_int", func(t *testing.T) { check(t, "agent.max_turns", "-5", -5, "agent", "max_turns") })
	t.Run("test_whitespace_padded_int", func(t *testing.T) { check(t, "agent.max_turns", " 42 ", 42, "agent", "max_turns") })
	t.Run("test_negative_float", func(t *testing.T) { check(t, "agent.max_turns", "-2.5", -2.5, "agent", "max_turns") })
	t.Run("test_lossy_decimal_identifier_stays_string", func(t *testing.T) {
		check(t, "mcp_servers.example.oauth.client_id", "123456789012.98765432109876", "123456789012.98765432109876",
			"mcp_servers", "example", "oauth", "client_id")
	})
}

func TestNullCoercion(t *testing.T) {
	t.Run("test_null_tokens_coerce_to_none", func(t *testing.T) {
		for _, token := range []string{"null", "none", "None", "~"} {
			if got, ok := setAndRead(t, "agent.run_budget_seconds", token, "agent", "run_budget_seconds"); !ok || got != nil {
				t.Fatalf("%q saved %#v", token, got)
			}
		}
	})
}

func TestMalformedKey(t *testing.T) {
	t.Run("test_empty_segment_rejected", func(t *testing.T) {
		for _, bad := range []string{"agent.", ".agent", "agent..max_turns", "  "} {
			h, _ := facadeHome(t, "")
			captureFacade(t)
			if code := exitCode(SetConfigValue(context.Background(), h, bad, "5", false, testCommandSources())); code != 1 {
				t.Fatalf("%q exit %d", bad, code)
			}
		}
	})
}

func TestStringTypedGuardPreserved(t *testing.T) {
	t.Run("test_enum_off_stays_string", func(t *testing.T) {
		if got, ok := setAndRead(t, "approvals.mode", "off", "approvals", "mode"); !ok || got != "off" {
			t.Fatalf("saved %#v", got)
		}
	})
}

func TestConfigSetRefusesWrongContainerShapeAndKeepsModelShorthand(t *testing.T) {
	ctx := context.Background()
	h, path := facadeHome(t, "model: gpt-x\n")
	_, errOut := captureFacade(t)
	if code := exitCode(SetConfigValue(ctx, h, "agent.disabled_toolsets", "{a: 1}", false, testCommandSources())); code != 1 {
		t.Fatalf("mapping into list slot exit %d", code)
	}
	if !strings.Contains(errOut.String(), "must be a list, got a dict") {
		t.Fatalf("refusal text %q", errOut.String())
	}
	if err := SetConfigValue(ctx, h, "agent.disabled_toolsets", "web", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if got, _ := readSaved(t, path, "agent", "disabled_toolsets"); !pyEq(got, []any{"web"}) {
		t.Fatalf("scalar list slot saved %#v", got)
	}
	if err := SetConfigValue(ctx, h, "model.provider", "openrouter", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if got, _ := readSaved(t, path, "model", "default"); got != "gpt-x" {
		t.Fatalf("model shorthand lost, default %#v", got)
	}
}

func TestConfigGetMasksSecretsAndUnsetReportsMissing(t *testing.T) {
	ctx := context.Background()
	h, _ := facadeHome(t, "model:\n  api_key: sk-abcdefghijklmnop\n  token_count: 5\n")
	out, _ := captureFacade(t)
	if err := GetConfigValue(ctx, h, "model.api_key", false, false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "sk-a...mnop" {
		t.Fatalf("masked get %q", got)
	}
	out.Reset()
	if err := GetConfigValue(ctx, h, "model.api_key", false, true, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "sk-abcdefghijklmnop" {
		t.Fatalf("raw get %q", got)
	}
	if code := exitCode(UnsetConfigValue(ctx, h, "display.nothing_here", testCommandSources())); code != 1 {
		t.Fatalf("unset missing exit %d", code)
	}
	if err := UnsetConfigValue(ctx, h, "model.token_count", testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(ConfigCommand(ctx, h, ConfigArgs{Command: "bogus"}, testCommandSources())); code != 1 {
		t.Fatalf("unknown subcommand exit %d", code)
	}
}

func TestRedactConfigValueMasksCredentialLeavesOnly(t *testing.T) {
	in := map[string]any{"token_count": "123456789012345", "headers": map[string]any{"X-API-Key": "abcdefghijklmnopq"},
		"api_key": "${OPENAI_API_KEY}", "list": []any{map[string]any{"password": "short"}}}
	got := RedactConfigValue(in).(map[string]any)
	if got["token_count"] != "123456789012345" || got["api_key"] != "${OPENAI_API_KEY}" {
		t.Fatalf("visible values changed %#v", got)
	}
	if got["headers"].(map[string]any)["X-API-Key"] != "abcd...nopq" || got["list"].([]any)[0].(map[string]any)["password"] != "***" {
		t.Fatalf("secrets not masked %#v", got)
	}
	if CustomEndpointKeyEnv("10.0.0.5:8080") != "ICEBATH_CUSTOM_10_0_0_5_8080_API_KEY" || CustomEndpointKeyEnv("") != "ICEBATH_CUSTOM_API_KEY" {
		t.Fatal("custom endpoint key env")
	}
}

func TestSanitizeEnvFileCountsChangedLinesAndKeepsNormalizedFiles(t *testing.T) {
	h, _ := facadeHome(t, "")
	envPath := filepath.Join(h, ".env")
	os.WriteFile(envPath, []byte("A=1\r\n  B=2  \n# keep  \nC=3"), 0o600)
	fixes, err := SanitizeEnvFile(context.Background(), h)
	if err != nil || fixes != 2 {
		t.Fatalf("fixes %d err %v", fixes, err)
	}
	data, _ := os.ReadFile(envPath)
	if string(data) != "A=1\nB=2\n# keep  \nC=3\n" {
		t.Fatalf("sanitized %q", data)
	}
	if fixes, _ := SanitizeEnvFile(context.Background(), h); fixes != 0 {
		t.Fatalf("second pass fixes %d", fixes)
	}
}

func TestGetEnvValuePreferDotenvYieldsUnresolvedOpReferenceToScope(t *testing.T) {
	h, _ := facadeHome(t, "")
	os.WriteFile(filepath.Join(h, ".env"), []byte("FOO_KEY=op://vault/item\nBAR_KEY=dotenv\n"), 0o600)
	t.Setenv("FOO_KEY", "resolved")
	t.Setenv("BAR_KEY", "shell")
	ctx := context.Background()
	if v, _ := GetEnvValuePreferDotenv(ctx, h, "FOO_KEY"); v != "resolved" {
		t.Fatalf("op ref %q", v)
	}
	if v, _ := GetEnvValuePreferDotenv(ctx, h, "BAR_KEY"); v != "dotenv" {
		t.Fatalf("dotenv first %q", v)
	}
	if v, ok, _ := GetEnvValue(ctx, h, "BAR_KEY"); !ok || v != "shell" {
		t.Fatalf("process first %q", v)
	}
}

func TestMigrateConfigStampsLatestAndRefusesBelowFloor(t *testing.T) {
	ctx := context.Background()
	h, path := facadeHome(t, "_config_version: 3\nagent:\n  max_turns: 7\n")
	_, errOut := captureFacade(t)
	res, err := MigrateConfig(ctx, h, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(errOut.String(), "icebath config: This config predates version 12") {
		t.Fatalf("floor refusal %v %q", res.Warnings, errOut.String())
	}
	if v, _ := readSaved(t, path, "_config_version"); !pyEq(v, 3) {
		t.Fatalf("refused config rewritten to %v", v)
	}
	h, path = facadeHome(t, "agent:\n  max_turns: 7\n")
	if _, err := MigrateConfig(ctx, h, false, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := readSaved(t, path, "_config_version"); !pyEq(v, latestConfigVersion()) {
		t.Fatalf("unversioned config stamped %v", v)
	}
	if v, _ := readSaved(t, path, "agent", "max_turns"); !pyEq(v, 7) {
		t.Fatalf("user value lost %v", v)
	}
}

func TestSaveConfigReleasesTheLockWhenAFirstStampFails(t *testing.T) {
	ctx := context.Background()
	h, _ := facadeHome(t, "")
	RegisterConfigFacade(FacadeHooks{})
	if err := SaveConfig(ctx, h, map[string]any{"_config_version": 3}, SaveOptions{}); !errors.Is(err, ErrStampHooksUnregistered) {
		t.Fatalf("unregistered first stamp returned %v", err)
	}
	RegisterConfigFacade(FacadeHooks{Stamp: func() StampHooks {
		return StampHooks{
			CoreVersions:         []int{1},
			RecordCoreUse:        func(string, map[string]any, int) bool { panic("record failed") },
			AdoptAgentBrowserEnv: func(string, map[string]any) []string { return nil },
		}
	}})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("collaborator panic was swallowed")
			}
		}()
		_ = SaveConfig(ctx, h, map[string]any{"_config_version": 3}, SaveOptions{})
	}()
	done := make(chan error, 1)
	go func() {
		_, err := LoadContext(ctx, h, facadeLookup)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("config lock still held after a panicking save")
	}
}
