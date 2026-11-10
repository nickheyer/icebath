package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const RelayPluginsTOMLName = "relay-plugins.toml"

var relayTrue = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Outcome of migrating one profile home, empty strings standing for None
type RelayMigrationResult struct {
	Home            string
	TOMLPath        string
	MigratedVars    []string
	SkippedReason   string
	ValidationError string
	Diagnostics     []any
}

// Reports whether the home now selects a generated toml
func (r *RelayMigrationResult) Migrated() bool { return r.TOMLPath != "" && r.SkippedReason == "" }

// Python str of a value or empty for a falsy one, stripped
func relayText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

func relayTruthy(v any) bool { return relayTrue[strings.ToLower(relayText(v))] }

// Stripped env value or the default when blank
func relayEnvString(env map[string]any, name, fallback string) string {
	if s := relayText(env[name]); s != "" {
		return s
	}
	return fallback
}

// Ordered object from alternating keys and values
func relayObject(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Relay 0.11 plugins toml document equivalent to the legacy exporter variables, an ImportError without the binding
func RelayPluginPayloadFromLegacyEnv(env map[string]any) (*pyjson.Object, error) {
	if _, err := loadNemoRelayBinding(); err != nil {
		return nil, err
	}
	cfg := relayObject("version", 4)
	if relayTruthy(env["ICEBATH_NEMO_RELAY_ATOF_ENABLED"]) {
		mode := strings.ToLower(relayEnvString(env, "ICEBATH_NEMO_RELAY_ATOF_MODE", "append"))
		if mode != "overwrite" {
			mode = "append"
		}
		sink := relayObject("type", "file")
		if dir := relayEnvString(env, "ICEBATH_NEMO_RELAY_ATOF_OUTPUT_DIRECTORY", ""); dir != "" {
			sink.Set("output_directory", dir)
		}
		sink.Set("filename", relayEnvString(env, "ICEBATH_NEMO_RELAY_ATOF_FILENAME", "icebath-atof.jsonl"))
		sink.Set("mode", mode)
		cfg.Set("atof", relayObject("enabled", true, "sinks", []any{sink}))
	}
	if relayTruthy(env["ICEBATH_NEMO_RELAY_ATIF_ENABLED"]) {
		atif := relayObject("enabled", true, "agent_name", relayEnvString(env, "ICEBATH_NEMO_RELAY_ATIF_AGENT_NAME", "Icebath Agent"))
		if v := relayEnvString(env, "ICEBATH_NEMO_RELAY_ATIF_AGENT_VERSION", ""); v != "" {
			atif.Set("agent_version", v)
		}
		atif.Set("model_name", relayEnvString(env, "ICEBATH_NEMO_RELAY_ATIF_MODEL_NAME", "unknown"))
		if dir := relayEnvString(env, "ICEBATH_NEMO_RELAY_ATIF_OUTPUT_DIRECTORY", ""); dir != "" {
			atif.Set("output_directory", dir)
		}
		atif.Set("filename_template", relayEnvString(env, "ICEBATH_NEMO_RELAY_ATIF_FILENAME_TEMPLATE", "icebath-atif-{session_id}.json"))
		atif.Set("include_observed_events", true)
		atif.Set("include_llm_requests", true)
		cfg.Set("atif", atif)
	}
	cfg.Set("opentelemetry", relayObject("enabled", false, "endpoints", []any{}, "file_sinks", []any{}))
	cfg.Set("policy", relayObject("unknown_component", "warn", "unknown_field", "warn", "unsupported_value", "error"))
	cfg.Set("enable_full_payloads", false)
	component := relayObject("kind", "observability", "enabled", true, "config", cfg)
	return relayObject("version", 1, "components", []any{component}), nil
}

// Toml text of the document as tomli_w writes it
func DumpsTOML(document *pyjson.Object) (string, error) { return pytoml.Dumps(document) }

// Plain maps and lists of an ordered document, the dict Relay receives
func relayPlain(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			out[k] = relayPlain(t.Vals[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = relayPlain(item)
		}
		return out
	}
	return v
}

// Relay warning diagnostics of the payload, a ValueError joining the messages of any error level diagnostic
func ValidateRelayPluginPayload(ctx context.Context, payload *pyjson.Object) ([]any, error) {
	relay, err := loadNemoRelayBinding()
	if err != nil {
		return nil, err
	}
	report, err := relayValidateExact(ctx, relay, relayPlain(payload).(map[string]any))
	if err != nil {
		return nil, err
	}
	rawConfig, ok := report["config"]
	if !ok {
		return nil, &HostBuiltinError{Type: "KeyError", Message: "'config'"}
	}
	cfg, ok := rawConfig.(map[string]any)
	if !ok {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(rawConfig))}
	}
	rawDiagnostics, ok := cfg["diagnostics"]
	if !ok {
		return nil, &HostBuiltinError{Type: "KeyError", Message: "'diagnostics'"}
	}
	diagnostics, ok := rawDiagnostics.([]any)
	if !ok {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(rawDiagnostics))}
	}
	var messages []string
	for _, d := range diagnostics {
		diag, ok := d.(map[string]any)
		if !ok {
			return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(d))}
		}
		if !pyrepr.Equal(diag["level"], "error") {
			continue
		}
		switch {
		case pyrepr.Truthy(diag["message"]):
			messages = append(messages, pyrepr.Str(diag["message"]))
		case pyrepr.Truthy(diag["code"]):
			messages = append(messages, pyrepr.Str(diag["code"]))
		default:
			messages = append(messages, pyrepr.Str(diag))
		}
	}
	if len(messages) > 0 {
		return nil, &HostBuiltinError{Type: "ValueError", Message: strings.Join(messages, "; ")}
	}
	return append([]any{}, diagnostics...), nil
}

// Exception class and message as the type name colon text Python formats
func relayExceptionText(err error) string {
	var builtin *HostBuiltinError
	var relayErr *tools.RelayError
	switch {
	case errors.As(err, &builtin):
		return builtin.Type + ": " + builtin.Message
	case errors.As(err, &relayErr):
		return relayErr.Class + ": " + relayErr.Message
	}
	return "Exception: " + err.Error()
}

// Lines assigning any of names become comments pointing at the generated toml
func relayCommentOutLegacyLines(lines []string, names []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		defined := false
		for _, name := range names {
			defined = defined || relayEnvLineDefinesKey(line, name)
		}
		if defined {
			out = append(out, "# migrated to "+RelayPluginsTOMLName+": "+pystr.Rstrip(line)+"\n")
		} else {
			out = append(out, line)
		}
	}
	return out
}

// Migrates one profile home env file, a Relay failure leaving it untouched and reported in ValidationError
func MigrateProfileRelayEnv(ctx context.Context, homeDir string, validate bool) (*RelayMigrationResult, error) {
	result := &RelayMigrationResult{Home: homeDir}
	envPath := filepath.Join(homeDir, ".env")
	if !isFile(envPath) {
		result.SkippedReason = "no .env"
		return result, nil
	}
	lines, err := relayEnvReadLines(envPath)
	if err != nil {
		return nil, err
	}
	env := map[string]any{}
	for _, line := range lines {
		stripped := pystr.Strip(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") || !strings.Contains(stripped, "=") {
			continue
		}
		key, value, _ := strings.Cut(strings.TrimPrefix(stripped, "export "), "=")
		env[pystr.Strip(key)] = pystr.StripChars(pystr.Strip(value), "'\"")
	}
	legacy := tools.ConfiguredLegacyRelayEnvVars(env)
	if len(legacy) == 0 {
		result.SkippedReason = "no legacy exporter variables"
		return result, nil
	}
	if selected, _ := env[tools.RelayPluginsConfigEnv].(string); pystr.Strip(selected) != "" {
		result.SkippedReason = tools.RelayPluginsConfigEnv + " already set"
		return result, nil
	}
	if !relayTruthy(env["ICEBATH_NEMO_RELAY_ATIF_ENABLED"]) && !relayTruthy(env["ICEBATH_NEMO_RELAY_ATOF_ENABLED"]) {
		result.SkippedReason = "no exporter enabled by the legacy variables"
		return result, nil
	}
	payload, err := RelayPluginPayloadFromLegacyEnv(env)
	if err == nil && validate {
		result.Diagnostics, err = ValidateRelayPluginPayload(ctx, payload)
	}
	if err != nil {
		result.ValidationError = relayExceptionText(err)
		return result, nil
	}
	tomlPath := filepath.Join(homeDir, RelayPluginsTOMLName)
	header := "# NeMo Relay plugin configuration for Icebath (selected via " + tools.RelayPluginsConfigEnv + " in .env).\n" +
		"# Generated by `icebath update` from the legacy ICEBATH_NEMO_RELAY_ATIF_*/ATOF_* variables,\n" +
		"# which Relay no longer reads. Edit this file to change exporters.\n"
	text, err := DumpsTOML(payload)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(tomlPath, []byte(header+text), 0o666); err != nil {
		return nil, err
	}
	newLines := relayCommentOutLegacyLines(lines, tools.LegacyRelayExportEnvVars)
	selects := false
	for _, line := range newLines {
		selects = selects || relayEnvLineDefinesKey(line, tools.RelayPluginsConfigEnv)
	}
	if !selects {
		if n := len(newLines); n > 0 && !strings.HasSuffix(newLines[n-1], "\n") {
			newLines[n-1] += "\n"
		}
		newLines = append(newLines, tools.RelayPluginsConfigEnv+"="+config.QuoteEnvValue(tomlPath)+"\n")
	}
	if err := relayEnvWriteLines(ctx, envPath, newLines); err != nil {
		return nil, err
	}
	result.TOMLPath = tomlPath
	result.MigratedVars = legacy
	return result, nil
}

// Migrates the default home and every live named profile, each keeping its own toml
func MigrateAllProfileRelayEnvs(ctx context.Context, validate bool) ([]*RelayMigrationResult, error) {
	named, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	var results []*RelayMigrationResult
	for _, h := range append([]string{profiles.DefaultHome()}, named...) {
		r, err := MigrateProfileRelayEnv(ctx, h, validate)
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}

// Prints the migrated profiles and the failed ones with the manual fix, nothing when neither exists
func PrintRelayMigrationReport(w io.Writer, results []*RelayMigrationResult) {
	var migrated, failed []*RelayMigrationResult
	for _, r := range results {
		if r.Migrated() {
			migrated = append(migrated, r)
		}
		if r.ValidationError != "" {
			failed = append(failed, r)
		}
	}
	if len(migrated) == 0 && len(failed) == 0 {
		return
	}
	fmt.Fprintln(w)
	if len(migrated) > 0 {
		fmt.Fprintln(w, "\033[1;33m⚠  NeMo Relay exporter configuration migrated\033[0m")
		fmt.Fprintln(w, "   Relay no longer reads the legacy ICEBATH_NEMO_RELAY_ATIF_*/ATOF_* variables.")
		fmt.Fprintln(w, "   Each profile below now has a generated relay-plugins.toml selected by")
		fmt.Fprintf(w, "   %s in its .env (legacy lines commented out, not deleted):\n", tools.RelayPluginsConfigEnv)
		for _, r := range migrated {
			extra := ""
			if len(r.Diagnostics) > 0 {
				extra = fmt.Sprintf(" (%d Relay diagnostic(s))", len(r.Diagnostics))
			}
			label := "default"
			if filepath.Base(filepath.Dir(r.Home)) == "profiles" {
				label = filepath.Base(r.Home)
			}
			fmt.Fprintf(w, "     • %s: %s%s\n", label, r.TOMLPath, extra)
		}
		fmt.Fprintln(w, "   Restart the gateway to apply the migrated exporter settings. Review the file and adjust paths if needed.")
	}
	for _, r := range failed {
		fmt.Fprintf(w, "   ✗ %s: could not migrate Relay exporter vars — %s\n", r.Home, r.ValidationError)
		fmt.Fprintf(w, "     Write %s by hand and set %s.\n", filepath.Join(r.Home, RelayPluginsTOMLName), tools.RelayPluginsConfigEnv)
	}
}

// Update hook migrating every profile home and printing the notice
func RunRelayMigrationAfterUpdate(ctx context.Context, w io.Writer) error {
	results, err := MigrateAllProfileRelayEnvs(ctx, true)
	if err != nil {
		return err
	}
	PrintRelayMigrationReport(w, results)
	return nil
}

// Migrates the active home or every profile, printing the report and each home with nothing to migrate
func CmdMigrateRelay(ctx context.Context, w io.Writer, allProfiles, noValidate bool) error {
	var results []*RelayMigrationResult
	if allProfiles {
		all, err := MigrateAllProfileRelayEnvs(ctx, !noValidate)
		if err != nil {
			return err
		}
		results = all
	} else {
		r, err := MigrateProfileRelayEnv(ctx, home.Dir(ctx), !noValidate)
		if err != nil {
			return err
		}
		results = []*RelayMigrationResult{r}
	}
	PrintRelayMigrationReport(w, results)
	for _, r := range results {
		if !r.Migrated() && r.ValidationError == "" {
			fmt.Fprintf(w, "  %s: nothing to migrate (%s).\n", r.Home, r.SkippedReason)
		}
	}
	return nil
}

// Native Relay binding the nemo_relay import yields, a ModuleNotFoundError when no binding is registered
func loadNemoRelayBinding() (*tools.NemoRelay, error) {
	relay, err := tools.LoadNemoRelay()
	var relayErr *tools.RelayError
	switch {
	case errors.As(err, &relayErr):
		return nil, err
	case err != nil:
		return nil, &HostBuiltinError{Type: "ModuleNotFoundError", Message: err.Error()}
	case relay == nil:
		return nil, &HostBuiltinError{Type: "ModuleNotFoundError", Message: "No module named 'nemo_relay'"}
	}
	return relay, nil
}

// Relay plugin.validate_exact report of the payload alone, never layered over the ambient user config
func relayValidateExact(ctx context.Context, relay *tools.NemoRelay, payload map[string]any) (map[string]any, error) {
	switch {
	case relay == nil:
		return nil, &HostBuiltinError{Type: "ModuleNotFoundError", Message: "No module named 'nemo_relay'"}
	case relay.Plugin == nil:
		return nil, &HostBuiltinError{Type: "AttributeError", Message: "'NemoRelay' object has no attribute 'plugin'"}
	case relay.Plugin.ValidateExact == nil:
		return nil, &HostBuiltinError{Type: "AttributeError", Message: "'NemoRelayPlugin' object has no attribute 'validate_exact'"}
	}
	return relay.Plugin.ValidateExact(ctx, payload)
}

// Dotenv lines of the file as the config env reader normalizes them
func relayEnvReadLines(path string) ([]string, error) { return config.ReadEnvLines(path) }

// Rewrites the dotenv file with lines, keeping its existing mode
func relayEnvWriteLines(ctx context.Context, path string, lines []string) error {
	return config.WriteEnvLines(ctx, path, lines, true)
}

// Reports whether the dotenv line assigns key the way the env loader parses it
func relayEnvLineDefinesKey(line, key string) bool { return config.EnvLineDefinesKey(line, key) }
