package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pytoml"
	"github.com/nickheyer/icebath/pkg/tools"
)

const relayLegacyEnv = `OPENAI_API_KEY=sk-test
ICEBATH_NEMO_RELAY_ATOF_ENABLED=1
ICEBATH_NEMO_RELAY_ATOF_OUTPUT_DIRECTORY={home}/telemetry/atof
ICEBATH_NEMO_RELAY_ATOF_FILENAME=icebath-atof.jsonl
ICEBATH_NEMO_RELAY_ATOF_MODE=append
ICEBATH_NEMO_RELAY_ATIF_ENABLED=1
ICEBATH_NEMO_RELAY_ATIF_OUTPUT_DIRECTORY={home}/telemetry/atif
ICEBATH_NEMO_RELAY_ATIF_FILENAME_TEMPLATE=trajectory-{session_id}.json
ICEBATH_NEMO_RELAY_ATIF_SUBAGENT_EXPORT_MODE=all
`

func relayEnvText(h string) string { return strings.ReplaceAll(relayLegacyEnv, "{home}", h) }

// Key value pairs of the assignment lines of an env file
func relayParseEnv(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
			k, v, _ := strings.Cut(line, "=")
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"")
		}
	}
	return out
}

// Profile home under a fake OS home selected through ICEBATH_HOME
func relayProfileEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func relayReport(diagnostics ...any) map[string]any {
	return map[string]any{"config": map[string]any{"diagnostics": append([]any{}, diagnostics...)}, "config_paths": []any{}, "dynamic_plugins": []any{}}
}

// Canonical Relay 0.11 ComponentSpec document the legacy variables of h produce
func relayExpectedPayload(h string) map[string]any {
	return map[string]any{"version": 1, "components": []any{map[string]any{"kind": "observability", "enabled": true, "config": map[string]any{
		"version": 4,
		"atof":    map[string]any{"enabled": true, "sinks": []any{map[string]any{"type": "file", "output_directory": h + "/telemetry/atof", "filename": "icebath-atof.jsonl", "mode": "append"}}},
		"atif": map[string]any{"enabled": true, "agent_name": "Icebath Agent", "model_name": "unknown", "output_directory": h + "/telemetry/atif",
			"filename_template": "trajectory-{session_id}.json", "include_observed_events": true, "include_llm_requests": true},
		"opentelemetry":        map[string]any{"enabled": false, "endpoints": []any{}, "file_sinks": []any{}},
		"policy":               map[string]any{"unknown_component": "warn", "unknown_field": "warn", "unsupported_value": "error"},
		"enable_full_payloads": false,
	}}}}
}

func writeRelayFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, text)
}

func TestLegacyEnvBecomesValidatedTOMLSelectedFromEnv(t *testing.T) {
	h := relayProfileEnv(t)
	writeRelayFile(t, filepath.Join(h, ".env"), relayEnvText(h))
	var validated []map[string]any
	installRelayValidator(t, func(payload map[string]any) (map[string]any, error) {
		validated = append(validated, payload)
		return relayReport(), nil
	})
	result, err := MigrateProfileRelayEnv(context.Background(), h, true)
	if err != nil || !result.Migrated() {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(result.Diagnostics) != 0 || len(validated) != 1 || !reflect.DeepEqual(validated[0], relayExpectedPayload(h)) {
		t.Fatalf("diagnostics %v validated %#v", result.Diagnostics, validated)
	}
	tomlPath := filepath.Join(h, RelayPluginsTOMLName)
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := pytoml.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	cfg := document["components"].([]any)[0].(map[string]any)["config"].(map[string]any)
	sink := cfg["atof"].(map[string]any)["sinks"].([]any)[0].(map[string]any)
	if sink["type"] != "file" || sink["filename"] != "icebath-atof.jsonl" || cfg["atif"].(map[string]any)["filename_template"] != "trajectory-{session_id}.json" {
		t.Fatalf("document %v", document)
	}
	if cfg["opentelemetry"].(map[string]any)["enabled"] != false {
		t.Fatalf("opentelemetry %v", cfg["opentelemetry"])
	}
	if !strings.HasPrefix(string(data), "# NeMo Relay plugin configuration for Icebath (selected via "+tools.RelayPluginsConfigEnv+" in .env).\n") {
		t.Fatalf("header %q", string(data))
	}
	env := relayParseEnv(t, filepath.Join(h, ".env"))
	if env[tools.RelayPluginsConfigEnv] != tomlPath || env["OPENAI_API_KEY"] != "sk-test" || len(tools.ConfiguredLegacyRelayEnvVars(env)) != 0 {
		t.Fatalf("env %v", env)
	}
	raw, _ := os.ReadFile(filepath.Join(h, ".env"))
	if !strings.Contains(string(raw), "# migrated to relay-plugins.toml: ICEBATH_NEMO_RELAY_ATOF_ENABLED=1") {
		t.Fatalf(".env %q", raw)
	}
	var out bytes.Buffer
	PrintRelayMigrationReport(&out, []*RelayMigrationResult{result})
	if !strings.Contains(out.String(), "     • default: "+tomlPath+"\n") {
		t.Fatalf("report %q", out.String())
	}
	again, err := MigrateProfileRelayEnv(context.Background(), h, true)
	if err != nil || again.SkippedReason != "no legacy exporter variables" {
		t.Fatalf("second run %+v %v", again, err)
	}
	out.Reset()
	if err := CmdMigrateRelay(homeCtx(h), &out, false, false); err != nil || out.String() != "  "+h+": nothing to migrate (no legacy exporter variables).\n" {
		t.Fatalf("cmd %q %v", out.String(), err)
	}
}

func TestUpdateMigratesEveryProfileHomeSeparately(t *testing.T) {
	h := relayProfileEnv(t)
	installRelayValidator(t, func(map[string]any) (map[string]any, error) { t.Fatal("validate=False validated"); return nil, nil })
	writeRelayFile(t, filepath.Join(h, ".env"), "OPENAI_API_KEY=x\n")
	work, idle := filepath.Join(h, "profiles", "work"), filepath.Join(h, "profiles", "idle")
	writeRelayFile(t, filepath.Join(work, ".env"), relayEnvText(work))
	writeRelayFile(t, filepath.Join(idle, ".env"), "SLACK_BOT_TOKEN=y\n")
	all, err := MigrateAllProfileRelayEnvs(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]*RelayMigrationResult{}
	for _, r := range all {
		results[r.Home] = r
	}
	if r := results[work]; r == nil || !r.Migrated() || r.TOMLPath != filepath.Join(work, RelayPluginsTOMLName) {
		t.Fatalf("work %+v", r)
	}
	for _, other := range []string{idle, h} {
		if r := results[other]; r == nil || r.Migrated() {
			t.Fatalf("%s %+v", other, r)
		}
		if _, err := os.Stat(filepath.Join(other, RelayPluginsTOMLName)); !os.IsNotExist(err) {
			t.Fatalf("%s toml %v", other, err)
		}
	}
	if env := relayParseEnv(t, filepath.Join(work, ".env")); env[tools.RelayPluginsConfigEnv] != filepath.Join(work, RelayPluginsTOMLName) {
		t.Fatalf("work env %v", env)
	}
	if data, _ := os.ReadFile(filepath.Join(idle, ".env")); string(data) != "SLACK_BOT_TOKEN=y\n" {
		t.Fatalf("idle env %q", data)
	}
	var out bytes.Buffer
	PrintRelayMigrationReport(&out, all)
	if !strings.Contains(out.String(), "     • work: "+filepath.Join(work, RelayPluginsTOMLName)+"\n") {
		t.Fatalf("report %q", out.String())
	}
}

func TestErrorDiagnosticsRejectThePayloadLikeRelay08Did(t *testing.T) {
	installRelayValidator(t, func(map[string]any) (map[string]any, error) {
		return relayReport(map[string]any{"level": "warning", "code": "unknown_field", "message": "unknown field 'x'"},
			map[string]any{"level": "error", "code": "unsupported_value", "message": "atof.mode 'nope' is unsupported"}), nil
	})
	_, err := ValidateRelayPluginPayload(context.Background(), relayObject("version", 1))
	if err == nil || relayExceptionText(err) != "ValueError: atof.mode 'nope' is unsupported" {
		t.Fatalf("err %v", err)
	}
}

func TestWarningDiagnosticsAreReturnedNotRaised(t *testing.T) {
	warning := map[string]any{"level": "warning", "code": "unknown_field", "message": "unknown field 'x'"}
	installRelayValidator(t, func(map[string]any) (map[string]any, error) { return relayReport(warning), nil })
	got, err := ValidateRelayPluginPayload(context.Background(), relayObject("version", 1))
	if err != nil || !reflect.DeepEqual(got, []any{warning}) {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestErrorDiagnosticsLeaveEnvUntouched(t *testing.T) {
	h := relayProfileEnv(t)
	writeRelayFile(t, filepath.Join(h, ".env"), relayEnvText(h))
	installRelayValidator(t, func(map[string]any) (map[string]any, error) {
		return relayReport(map[string]any{"level": "error", "code": "bad", "message": "rejected"}), nil
	})
	result, err := MigrateProfileRelayEnv(context.Background(), h, true)
	if err != nil || result.Migrated() || result.ValidationError != "ValueError: rejected" {
		t.Fatalf("result %+v err %v", result, err)
	}
	tools.RegisterNemoRelayBinding(nil)
	missing, err := MigrateProfileRelayEnv(context.Background(), h, false)
	if err != nil || missing.ValidationError != "ModuleNotFoundError: No module named 'nemo_relay'" {
		t.Fatalf("missing binding %+v err %v", missing, err)
	}
	if data, _ := os.ReadFile(filepath.Join(h, ".env")); string(data) != relayEnvText(h) {
		t.Fatalf(".env changed %q", data)
	}
	if _, err := os.Stat(filepath.Join(h, RelayPluginsTOMLName)); !os.IsNotExist(err) {
		t.Fatalf("toml written %v", err)
	}
	var out bytes.Buffer
	PrintRelayMigrationReport(&out, []*RelayMigrationResult{result})
	want := "\n   ✗ " + h + ": could not migrate Relay exporter vars — ValueError: rejected\n     Write " + filepath.Join(h, RelayPluginsTOMLName) + " by hand and set " + tools.RelayPluginsConfigEnv + ".\n"
	if out.String() != want {
		t.Fatalf("report %q", out.String())
	}
}
