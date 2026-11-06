package provider

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Runtimes the codex runtime switch accepts
var CodexValidRuntimes = []string{"auto", "codex_app_server"}

var codexRuntimeArgSynonyms = map[string]string{
	"on": "codex_app_server", "codex": "codex_app_server", "enable": "codex_app_server",
	"off": "auto", "default": "auto", "disable": "auto", "icebath": "auto"}

var codexToolsCallbackNote = []string{
	"Icebath tool callback registered: codex can now use web_search, web_extract, vision_analyze, image_generate, skill_view, skills_list, text_to_speech, kanban_* (worker + orchestrator) via MCP.",
	"  (delegate_task, memory, session_search, todo run only on the default Icebath runtime — they need the agent loop context.)"}

// CodexRuntimeStatus is the outcome of one codex runtime command, rendered per surface by the caller
type CodexRuntimeStatus struct {
	Success            bool
	NewValue           *string
	OldValue           *string
	Message            string
	RequiresNewSession bool
}

// Runtime named by the command argument, nil with no argument, an error line for an unknown name
func CodexRuntimeParseArgs(argString string) (*string, []string) {
	raw := strings.ToLower(strings.TrimSpace(argString))
	if raw == "" {
		return nil, nil
	}
	value := raw
	if synonym, ok := codexRuntimeArgSynonyms[raw]; ok {
		value = synonym
	}
	if slices.Contains(CodexValidRuntimes, value) {
		return &value, nil
	}
	return nil, []string{fmt.Sprintf("Unknown runtime %s. Use one of: auto, codex_app_server, on, off", pyrepr.Repr(raw))}
}

// Configured model.openai_runtime, auto when unset, empty or unrecognized
func GetCurrentCodexRuntime(cfg map[string]any) string {
	if cfg == nil {
		return "auto"
	}
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		return "auto"
	}
	value := ""
	if cxTruthy(modelCfg["openai_runtime"]) {
		value = strings.ToLower(strings.TrimSpace(pyrepr.Str(modelCfg["openai_runtime"])))
	}
	if slices.Contains(CodexValidRuntimes, value) {
		return value
	}
	return "auto"
}

// Configured model.codex_bin as one argv element, bare codex from PATH when unset
func GetConfiguredCodexBinary(cfg map[string]any) string {
	modelCfg, _ := cfg["model"].(map[string]any)
	value := ""
	if modelCfg != nil && cxTruthy(modelCfg["codex_bin"]) {
		value = strings.TrimSpace(pyrepr.Str(modelCfg["codex_bin"]))
	}
	if value == "" {
		return "codex"
	}
	return value
}

// Stores the runtime in the config in place and returns the previous one
func SetCodexRuntime(cfg map[string]any, newValue string) (string, error) {
	if !slices.Contains(CodexValidRuntimes, newValue) {
		return "", fmt.Errorf("invalid runtime %s; must be one of ('auto', 'codex_app_server')", pyrepr.Repr(newValue))
	}
	old := GetCurrentCodexRuntime(cfg)
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		modelCfg = map[string]any{}
		cfg["model"] = modelCfg
	}
	modelCfg["openai_runtime"] = newValue
	return old, nil
}

// Codex CLI install and version check, the version on success else the reason
func CheckCodexBinaryOK(codexBin string) (bool, string) {
	ok, message, err := CheckCodexBinary(codexBin, CodexMinVersion)
	if err != nil {
		return false, "codex check failed: " + err.Error()
	}
	return ok, message
}

// Migration and binary probe the switch calls, swappable like the module attributes
var (
	codexRuntimeMigrate     = MigrateCodexRuntimePlugins
	codexRuntimeCheckBinary = CheckCodexBinaryOK
)

// Runs the codex config migration and describes it, a failure becomes one warning line
func codexRuntimeMigrationLines(cfg map[string]any) (lines []string) {
	defer func() {
		if r := recover(); r != nil {
			lines = append(lines, fmt.Sprintf("\u26a0 MCP migration skipped: %v", r))
		}
	}()
	report, err := codexRuntimeMigrate(cfg, CodexMigrateOptions{})
	if err != nil {
		return append(lines, "⚠ MCP migration skipped: "+err.Error())
	}
	var userServers []string
	for _, s := range report.Migrated {
		if s != CodexToolsMCPServerName {
			userServers = append(userServers, s)
		}
	}
	if len(userServers) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d MCP server(s): %s", len(userServers), strings.Join(userServers, ", ")))
	}
	if len(report.MigratedPlugins) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d native Codex plugin(s): %s", len(report.MigratedPlugins), strings.Join(report.MigratedPlugins, ", ")))
	} else if report.PluginQueryError != "" {
		lines = append(lines, "Codex plugin discovery skipped: "+report.PluginQueryError)
	}
	if report.WrotePermissionsDefault != "" {
		lines = append(lines, fmt.Sprintf("Default sandbox: %s (no approval prompt on every write)", report.WrotePermissionsDefault))
	}
	if slices.Contains(report.Migrated, CodexToolsMCPServerName) {
		lines = append(lines, codexToolsCallbackNote...)
	}
	lines = append(lines, fmt.Sprintf("  (config: %s)", report.TargetPath))
	for _, e := range report.Errors {
		lines = append(lines, "⚠ MCP migration: "+e)
	}
	return lines
}

// Shows or switches the codex runtime, persisting through the callback and migrating on enable, an invalid name errors
func ApplyCodexRuntime(cfg map[string]any, newValue *string, persist func(map[string]any) error) (CodexRuntimeStatus, error) {
	current := GetCurrentCodexRuntime(cfg)
	codexBin := GetConfiguredCodexBinary(cfg)
	type binaryCheck struct {
		ok  bool
		ver string
	}
	cache := map[string]binaryCheck{}
	checkBinary := func(bin string) (bool, string) {
		if c, ok := cache[bin]; ok {
			return c.ok, c.ver
		}
		ok, ver := codexRuntimeCheckBinary(bin)
		cache[bin] = binaryCheck{ok, ver}
		return ok, ver
	}
	cur := current
	if newValue == nil {
		ok, ver := checkBinary(codexBin)
		status := ""
		if ok {
			status = "OK " + ver
		} else if ver != "" {
			status = "not available — " + ver
		} else {
			status = "not available — install with `npm i -g @openai/codex`"
		}
		return CodexRuntimeStatus{Success: true, NewValue: &cur, OldValue: &cur, Message: fmt.Sprintf("openai_runtime: %s\ncodex CLI: %s", current, status)}, nil
	}
	value := *newValue
	reapplyingEnable := value == current && current == "codex_app_server"
	if value == current && !reapplyingEnable {
		return CodexRuntimeStatus{Success: true, NewValue: &cur, OldValue: &cur, Message: "openai_runtime already set to " + current}, nil
	}
	if value == "codex_app_server" {
		if ok, verOrMsg := checkBinary(codexBin); !ok {
			if verOrMsg == "" {
				verOrMsg = "codex CLI not available"
			}
			return CodexRuntimeStatus{Success: false, OldValue: &cur, Message: "Cannot enable codex_app_server runtime: " + verOrMsg + "\nInstall with: npm i -g @openai/codex"}, nil
		}
	}
	if !reapplyingEnable {
		if _, err := SetCodexRuntime(cfg, value); err != nil {
			return CodexRuntimeStatus{}, err
		}
		if persist != nil {
			if err := codexRuntimePersist(persist, cfg); err != nil {
				slog.Error("failed to persist openai_runtime change", "error", err)
				return CodexRuntimeStatus{Success: false, NewValue: &value, OldValue: &cur, Message: "updated config in memory but persist failed: " + err.Error()}, nil
			}
		}
	}
	var msg []string
	if reapplyingEnable {
		msg = append(msg, fmt.Sprintf("openai_runtime already set to %s — re-applying migration", current))
	} else {
		msg = append(msg, fmt.Sprintf("openai_runtime: %s → %s", current, value))
	}
	if value == "codex_app_server" {
		if ok, ver := checkBinary(codexBin); ok {
			msg = append(msg, "codex CLI: "+ver)
		}
		msg = append(msg, codexRuntimeMigrationLines(cfg)...)
		msg = append(msg, "OpenAI/Codex turns now run through `codex app-server` (terminal/file ops/patching inside Codex; Icebath tools available via MCP callback).")
		msg = append(msg, "Effective on next session — current cached agent keeps the prior runtime to preserve prompt cache.")
	} else {
		msg = append(msg, "OpenAI/Codex turns will use the default Icebath runtime.", "Effective on next session.")
	}
	return CodexRuntimeStatus{Success: true, NewValue: &value, OldValue: &cur, Message: strings.Join(msg, "\n"), RequiresNewSession: true}, nil
}

// Calls the persist callback, a panic in it reported as the raised exception
func codexRuntimePersist(persist func(map[string]any) error, cfg map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return persist(cfg)
}
