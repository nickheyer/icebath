package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var cliLog = logging.GetLogger("cli")

var (
	cliConfigMu     sync.Mutex
	cliConfigLoaded map[string]any
	gatewayProcess  atomic.Bool
)

// Marks this process as the one running the gateway so config mirroring leaves TERMINAL_CWD alone
func MarkGatewayProcess() { gatewayProcess.Store(true) }

// Reports whether this process runs the gateway
func InGatewayProcess() bool { return gatewayProcess.Load() }

// The process wide CLI config, loaded once from the home config over the built in defaults
func CLIConfig(ctx context.Context) map[string]any {
	cliConfigMu.Lock()
	defer cliConfigMu.Unlock()
	if cliConfigLoaded == nil {
		cliConfigLoaded = LoadCLIConfig(ctx)
	}
	return cliConfigLoaded
}

// Replaces the process wide CLI config, nil forcing the next CLIConfig call to reload
func SetCLIConfig(cfg map[string]any) {
	cliConfigMu.Lock()
	cliConfigLoaded = cfg
	cliConfigMu.Unlock()
}

// Loads prefill messages from a JSON array file, relative paths under baseDir or the home, missing or invalid giving none
func loadPrefillMessages(ctx context.Context, filePath, baseDir string) []any {
	if filePath == "" {
		return []any{}
	}
	path := pypath.Expanduser(filePath)
	if !filepath.IsAbs(path) {
		if baseDir == "" {
			baseDir = home.Dir(ctx)
		}
		path = filepath.Join(baseDir, path)
	}
	if _, err := os.Stat(path); err != nil {
		cliLog.Warning(fmt.Sprintf("Prefill messages file not found: %s", path))
		return []any{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		cliLog.Warning(fmt.Sprintf("Failed to load prefill messages from %s: %s", path, err))
		return []any{}
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		cliLog.Warning(fmt.Sprintf("Failed to load prefill messages from %s: %s", path, err))
		return []any{}
	}
	list, ok := pyjson.Plain(data).([]any)
	if !ok {
		cliLog.Warning(fmt.Sprintf("Prefill messages file must contain a JSON array: %s", path))
		return []any{}
	}
	return list
}

// Prefill file path from the session env, then the top level key, then the legacy agent key
func resolvePrefillMessagesFile(ctx context.Context, cfg map[string]any) string {
	if v := strings.TrimSpace(state.SessionEnv(ctx, "ICEBATH_PREFILL_MESSAGES_FILE", "")); v != "" {
		return v
	}
	if v := strings.TrimSpace(strOrEmpty(cfg["prefill_messages_file"])); v != "" {
		return v
	}
	if agentCfg, ok := cfg["agent"].(map[string]any); ok {
		return strings.TrimSpace(strOrEmpty(agentCfg["prefill_messages_file"]))
	}
	return ""
}

// Python str of a value, empty for None and for the empty string
func strOrEmpty(v any) string {
	if v == nil || !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Parses a reasoning effort level, warning once when a non empty value is unknown
func parseReasoningConfig(effort any) map[string]any {
	result := config.ParseReasoningEffort(effort)
	if pyrepr.Truthy(effort) && strings.TrimSpace(pyrepr.Str(effort)) != "" && result == nil {
		cliLog.Warning(fmt.Sprintf("Unknown reasoning_effort '%s', using default (medium)", pyrepr.Str(effort)))
	}
	return result
}

// Parses a persisted fast mode preference, warning when the word is neither a tier nor a normal tier word
func parseServiceTierConfig(raw string) string {
	tier := provider.ParseServiceTier(raw)
	if tier == "" && !provider.NormalTierWords[strings.ToLower(strings.TrimSpace(raw))] {
		cliLog.Warning(fmt.Sprintf("Unknown service_tier '%s', ignoring", raw))
	}
	return tier
}

var terminalEnvKeys = []string{
	"degraded_mode", "cwd", "timeout", "home_mode", "lifetime_seconds", "docker_image",
	"docker_forward_env", "modal_image", "daytona_image", "vercel_runtime", "vercel_image",
	"ssh_host", "ssh_user", "ssh_port", "ssh_key", "container_cpu", "container_memory",
	"container_disk", "container_persistent", "docker_volumes", "docker_env", "docker_extra_args",
	"docker_shm_size", "docker_mount_cwd_to_workspace", "docker_network", "docker_run_as_host_user",
	"docker_snap_compat",
	"docker_persist_across_processes", "docker_shared_container_key", "docker_orphan_reaper",
	"sandbox_dir", "persistent_shell",
}

// Ordered terminal config key to env var pairs, env_type first and sudo_password last
func terminalEnvMappings() [][2]string {
	out := [][2]string{{"env_type", "TERMINAL_ENV"}}
	for _, key := range terminalEnvKeys {
		out = append(out, [2]string{key, "TERMINAL_" + strings.ToUpper(key)})
	}
	return append(out, [2]string{"sudo_password", "SUDO_PASSWORD"})
}

var auxiliaryTaskEnv = []struct {
	task   string
	fields [][2]string
}{
	{"vision", [][2]string{{"provider", "AUXILIARY_VISION_PROVIDER"}, {"model", "AUXILIARY_VISION_MODEL"},
		{"base_url", "AUXILIARY_VISION_BASE_URL"}, {"api_key", "AUXILIARY_VISION_API_KEY"}}},
	{"approval", [][2]string{{"provider", "AUXILIARY_APPROVAL_PROVIDER"}, {"model", "AUXILIARY_APPROVAL_MODEL"},
		{"base_url", "AUXILIARY_APPROVAL_BASE_URL"}, {"api_key", "AUXILIARY_APPROVAL_API_KEY"}}},
}

var cwdStandIns = map[string]bool{".": true, "auto": true, "cwd": true}

// Dict valued section of a config, an empty map when missing or not a dict
func sectionOf(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Projects config values into the env vars the tool modules read, an already set env var winning
func mirrorConfigToEnv(defaults map[string]any, fileHasTerminalConfig bool) {
	terminalConfig := sectionOf(defaults, "terminal")
	if backend, ok := terminalConfig["backend"]; ok {
		terminalConfig["env_type"] = backend
	}
	effectiveBackend := "local"
	if v, ok := terminalConfig["env_type"]; ok {
		effectiveBackend = pyrepr.Str(v)
	}
	if effectiveBackend == "local" {
		cwd, _ := os.Getwd()
		terminalConfig["cwd"] = cwd
		if sec, ok := defaults["terminal"].(map[string]any); ok {
			sec["cwd"] = cwd
		} else {
			defaults["terminal"] = terminalConfig
		}
	} else if v, ok := terminalConfig["cwd"]; ok {
		if s, isStr := v.(string); isStr && cwdStandIns[s] {
			delete(terminalConfig, "cwd")
		}
	}
	isGateway := InGatewayProcess()
	for _, pair := range terminalEnvMappings() {
		val, ok := terminalConfig[pair[0]]
		if !ok {
			continue
		}
		envVar := pair[1]
		if envVar == "TERMINAL_CWD" {
			if !isGateway {
				os.Setenv(envVar, pyrepr.Str(val))
			}
			continue
		}
		if _, set := os.LookupEnv(envVar); fileHasTerminalConfig || !set {
			os.Setenv(envVar, envValueText(val))
		}
	}
	browserConfig := sectionOf(defaults, "browser")
	if v, ok := browserConfig["inactivity_timeout"]; ok {
		os.Setenv("BROWSER_INACTIVITY_TIMEOUT", pyrepr.Str(v))
	}
	auxiliaryConfig := sectionOf(defaults, "auxiliary")
	for _, task := range auxiliaryTaskEnv {
		taskCfg, ok := auxiliaryConfig[task.task].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range task.fields {
			val := strings.TrimSpace(pyrepr.Str(valueOr(taskCfg[field[0]], "")))
			if val != "" && !(field[0] == "provider" && val == "auto") {
				os.Setenv(field[1], val)
			}
		}
	}
	if securityConfig, ok := defaults["security"].(map[string]any); ok {
		if redact, present := securityConfig["redact_secrets"]; present && redact != nil {
			os.Setenv("ICEBATH_REDACT_SECRETS", strings.ToLower(pyrepr.Str(redact)))
		}
	}
	if sessionsConfig, ok := defaults["sessions"].(map[string]any); ok {
		if v, present := sessionsConfig["cjk_fts"]; present {
			os.Setenv("ICEBATH_CJK_FTS", pyrepr.Str(v))
		}
		if v, present := sessionsConfig["search_slow_ms"]; present {
			os.Setenv("ICEBATH_SEARCH_SLOW_MS", pyrepr.Str(v))
		}
	}
}

// The value, or def when the value is missing
func valueOr(v any, def any) any {
	if v == nil {
		return def
	}
	return v
}

// JSON text for a list or dict, Python str for everything else
func envValueText(val any) string {
	switch val.(type) {
	case []any, map[string]any:
		text, err := pyjson.Dumps(val)
		if err != nil {
			panic(err)
		}
		return text
	}
	return pyrepr.Str(val)
}

// Built in defaults for every config key the CLI reads
func cliConfigDefaults() map[string]any {
	img := config.DefaultSandboxImage
	return map[string]any{
		"model": map[string]any{"default": "", "base_url": "", "provider": "auto"},
		"terminal": map[string]any{
			"env_type": "local", "cwd": ".", "home_mode": "auto", "lifetime_seconds": 300,
			"docker_image": img, "docker_forward_env": []any{}, "modal_image": img, "daytona_image": img, "docker_volumes": []any{},
			"docker_mount_cwd_to_workspace": false,
			"docker_shared_container_key":   "",
		},
		"browser":     map[string]any{"inactivity_timeout": 120, "engine": "auto"},
		"compression": map[string]any{"enabled": true, "threshold": 0.50, "min_tail_user_messages": 1},
		"agent": map[string]any{
			"max_turns": 500, "verbose": false, "system_prompt": "", "prefill_messages_file": "",
			"reasoning_effort": "", "service_tier": "",
			"personalities": map[string]any{},
		},
		"display": map[string]any{
			"compact":        false,
			"resume_display": "full", "resume_exchanges": 10, "resume_max_user_chars": 300,
			"resume_max_assistant_chars": 200, "resume_max_assistant_lines": 3, "resume_skip_tool_only": true,
			"show_reasoning": true, "reasoning_full": false, "streaming": true, "busy_input_mode": "interrupt",
			"cli_layout":        "modern",
			"persistent_output": true, "persistent_output_max_lines": 200,
			"cli_rebuild_scrollback_on_redraw": false,
			"persist_prompts":                  true,
			"skin":                             "default",
		},
		"code_execution": map[string]any{"timeout": 300, "max_tool_calls": 50},
		"auxiliary":      map[string]any{"vision": map[string]any{"provider": "auto", "model": "", "base_url": "", "api_key": ""}},
		"delegation":     map[string]any{"max_iterations": 45, "model": "", "provider": "", "base_url": "", "api_key": ""},
		"onboarding":     map[string]any{"seen": map[string]any{}},
	}
}

// Overlays a parsed config file onto the defaults in place with model normalization, deep merge and legacy keys
func mergeFileConfig(defaults, fileConfig map[string]any) {
	modelDefaults := sectionOf(defaults, "model")
	defaults["model"] = modelDefaults
	if fileModel, ok := fileConfig["model"]; ok {
		switch m := fileModel.(type) {
		case string:
			modelDefaults["default"] = m
		case map[string]any:
			for k, v := range m {
				modelDefaults[k] = v
			}
			_, hasModel := m["model"]
			_, hasDefault := m["default"]
			if hasModel && !hasDefault {
				modelDefaults["default"] = m["model"]
			}
		}
	}
	for key, value := range fileConfig {
		if key == "model" {
			continue
		}
		if existing, ok := defaults[key].(map[string]any); ok {
			if section, isMap := value.(map[string]any); isMap {
				for k, v := range section {
					existing[k] = v
				}
			} else if value != nil {
				defaults[key] = value
			}
			continue
		}
		defaults[key] = value
	}
	agentFile, agentIsMap := fileConfig["agent"].(map[string]any)
	if rootTurns, ok := fileConfig["max_turns"]; ok && !(agentIsMap && agentFile["max_turns"] != nil) {
		sectionOf(defaults, "agent")["max_turns"] = rootTurns
	}
}

// Home config.yaml, else cli-config.yaml beside the executable, over the built in defaults, with env mirroring
func LoadCLIConfig(ctx context.Context) map[string]any {
	configPath := filepath.Join(home.Dir(ctx), "config.yaml")
	exists, _ := config.ConfigExists(ctx, configPath)
	if !exists || os.Getenv("ICEBATH_IGNORE_USER_CONFIG") == "1" {
		configPath = filepath.Join(executableDir(), "cli-config.yaml")
	}
	defaults := cliConfigDefaults()
	fileHasTerminalConfig := false
	if exists, _ = config.ConfigExists(ctx, configPath); exists {
		doc, err := config.ReadConfigDoc(ctx, configPath)
		if err != nil {
			cliLog.Warning(fmt.Sprintf("Failed to load cli-config.yaml: %s", err))
		} else {
			fileConfig, _ := doc.(map[string]any)
			if fileConfig == nil {
				fileConfig = map[string]any{}
			}
			fileConfig = config.NormalizeRootModelKeys(fileConfig)
			_, fileHasTerminalConfig = fileConfig["terminal"]
			mergeFileConfig(defaults, fileConfig)
		}
	}
	defaults = config.Expand(defaults, os.LookupEnv).(map[string]any)
	defaults = config.ApplyManagedOverlay(defaults)
	mirrorConfigToEnv(defaults, fileHasTerminalConfig)
	return defaults
}

// Directory holding the running executable, the working directory when it cannot be resolved
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}
	return filepath.Dir(exe)
}

// Startup side effects from the loaded config, logging, config warnings, skin and display knobs, each failure ignored
func initLoggingAndDisplayFromConfig(ctx context.Context) {
	cfg := CLIConfig(ctx)
	display := sectionOf(cfg, "display")
	steps := []func(){
		func() { logging.SetupLogging(ctx, logging.Options{Mode: "cli"}) },
		func() { config.PrintConfigWarnings(ctx, home.Dir(ctx), nil, tools.RemovedBackendNote) },
		func() { config.InitSkinFromConfig(ctx, cfg) },
		func() { tools.SetToolPreviewMaxLen(intOr(valueOr(display["tool_preview_length"], 0), 0)) },
		func() { tools.SetFriendlyToolLabels(pyrepr.Truthy(valueOr(display["friendly_tool_labels"], true))) },
	}
	for _, step := range steps {
		func() {
			defer func() { recover() }()
			step()
		}()
	}
}

// Python int() of a bool, integer, float or numeric string, false when it does not parse
func pyIntOf(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(x), true
	case float32:
		return pyIntOf(float64(x))
	case string:
		n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(x), "_", ""), 10, 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
		return int(n.Int64()), true
	}
	return 0, false
}

// Python int of the value, or def when it does not parse
func intOr(v any, def int) int {
	if n, ok := pyIntOf(v); ok {
		return n
	}
	return def
}
