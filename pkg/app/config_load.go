package app

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

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
		pkgLog().Warn(fmt.Sprintf("Prefill messages file not found: %s", path))
		return []any{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %s", path, err))
		return []any{}
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %s", path, err))
		return []any{}
	}
	list, ok := pyjson.Plain(data).([]any)
	if !ok {
		pkgLog().Warn(fmt.Sprintf("Prefill messages file must contain a JSON array: %s", path))
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
		pkgLog().Warn(fmt.Sprintf("Unknown reasoning_effort '%s', using default (medium)", pyrepr.Str(effort)))
	}
	return result
}

// Parses a persisted fast mode preference, warning when the word is neither a tier nor a normal tier word
func parseServiceTierConfig(raw string) string {
	tier := provider.ParseServiceTier(raw)
	if tier == "" && !provider.NormalTierWords[strings.ToLower(strings.TrimSpace(raw))] {
		pkgLog().Warn(fmt.Sprintf("Unknown service_tier '%s', ignoring", raw))
	}
	return tier
}

var cwdStandIns = map[string]bool{".": true, "auto": true, "cwd": true}

// Dict valued section of a config, an empty map when missing or not a dict
func sectionOf(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Hands the terminal tools this launch's working folder: the launch folder on the local backend, else the configured one
func exportTerminalCwd(cfg map[string]any) {
	terminal, ok := cfg["terminal"].(map[string]any)
	if !ok {
		terminal = map[string]any{}
		cfg["terminal"] = terminal
	}
	backend := "local"
	if v, set := terminal["backend"]; set && v != nil {
		backend = pyrepr.Str(v)
	}
	cwd := ""
	if backend == "local" {
		cwd, _ = os.Getwd()
		terminal["cwd"] = cwd
	} else if v, set := terminal["cwd"]; set {
		if text, isStr := v.(string); isStr && cwdStandIns[text] {
			delete(terminal, "cwd")
		} else {
			cwd = pyrepr.Str(v)
		}
	}
	if cwd != "" && !InGatewayProcess() {
		os.Setenv("TERMINAL_CWD", cwd)
	}
}

// The registry defaults every config starts from
func cliConfigDefaults() map[string]any {
	return config.Clone(map[string]any(config.Defaults())).(map[string]any)
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

// Home config.yaml, else cli-config.yaml beside the executable, over the registry defaults, the terminal working folder exported
func LoadCLIConfig(ctx context.Context) map[string]any {
	configPath := filepath.Join(home.Dir(ctx), "config.yaml")
	exists, _ := config.ConfigExists(ctx, configPath)
	if !exists || os.Getenv("ICEBATH_IGNORE_USER_CONFIG") == "1" {
		configPath = filepath.Join(executableDir(), "cli-config.yaml")
	}
	defaults := cliConfigDefaults()
	if exists, _ = config.ConfigExists(ctx, configPath); exists {
		doc, err := config.ReadConfigDoc(ctx, configPath)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("Failed to load cli-config.yaml: %s", err))
		} else {
			fileConfig, _ := doc.(map[string]any)
			if fileConfig == nil {
				fileConfig = map[string]any{}
			}
			mergeFileConfig(defaults, fileConfig)
		}
	}
	defaults = config.Expand(defaults, os.LookupEnv).(map[string]any)
	defaults = config.ApplyManagedOverlay(defaults)
	exportTerminalCwd(defaults)
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
		return pyIntString(x)
	}
	if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
		return int(n.Int64()), true
	}
	return 0, false
}

// Python int of a base ten string with Unicode digits and single underscores between digits, clamped to int
func pyIntString(s string) (int, bool) {
	s = pystr.Strip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if s == "" {
		return 0, false
	}
	digits := make([]byte, 0, len(s))
	afterUnderscore := true
	for _, r := range s {
		if r == '_' {
			if afterUnderscore {
				return 0, false
			}
			afterUnderscore = true
			continue
		}
		d, ok := pystr.DecimalValue(r)
		if !ok {
			return 0, false
		}
		digits = append(digits, byte('0'+d))
		afterUnderscore = false
	}
	if afterUnderscore {
		return 0, false
	}
	n, _ := new(big.Int).SetString(string(digits), 10)
	if neg {
		n.Neg(n)
	}
	switch {
	case n.IsInt64():
		return int(n.Int64()), true
	case neg:
		return math.MinInt, true
	}
	return math.MaxInt, true
}
