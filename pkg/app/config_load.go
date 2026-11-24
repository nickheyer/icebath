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

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// Registry defaults under the administrator's pins, the config document a run that ignores the user's config.yaml reads
func DefaultsConfig(ctx context.Context) map[string]any {
	return config.ApplyManagedOverlay(config.Clone(map[string]any(config.Defaults())).(map[string]any))
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

// Prefill file path for ctx: the session's client env, then the prefill messages setting
func resolvePrefillMessagesFile(ctx context.Context) string {
	if v := strings.TrimSpace(state.SessionEnv(ctx, "ICEBATH_PREFILL_MESSAGES_FILE", "")); v != "" {
		return v
	}
	return strings.TrimSpace(settings.PrefillMessagesFile.Get(ctx))
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

// Reasoning config the settings of ctx give a model, the configured model when none is named: its per model override, else the reasoning effort setting
func configuredReasoning(ctx context.Context, model string) map[string]any {
	if model == "" {
		model = strings.TrimSpace(settings.Model.Get(ctx))
	}
	if perModel := config.ResolvePerModelReasoningEffort(model, settings.AgentReasoningOverrides.Get(ctx)); perModel != nil {
		return perModel
	}
	return parseReasoningConfig(settings.ReasoningEffort.Get(ctx))
}

// Parses a persisted fast mode preference, warning when the word is neither a tier nor a normal tier word
func parseServiceTierConfig(raw string) string {
	tier := provider.ParseServiceTier(raw)
	if tier == "" && !provider.NormalTierWords[strings.ToLower(strings.TrimSpace(raw))] {
		pkgLog().Warn(fmt.Sprintf("Unknown service_tier '%s', ignoring", raw))
	}
	return tier
}

// Values of terminal.cwd that name no folder of their own
var cwdStandIns = map[string]bool{"": true, ".": true, "auto": true, "cwd": true}

// Folder this command line moved its sessions to: a worktree it made or the folder a resumed session recorded
var movedFolder struct {
	sync.Mutex
	path string
}

// Notes the folder this command line moved its sessions to
func moveCLIFolder(path string) {
	movedFolder.Lock()
	movedFolder.path = path
	movedFolder.Unlock()
}

// Folder this command line moved its sessions to, else the TERMINAL_CWD the process that started it handed it, empty for neither
func movedOrHandedFolder() string {
	movedFolder.Lock()
	moved := movedFolder.path
	movedFolder.Unlock()
	if moved != "" {
		return moved
	}
	return os.Getenv("TERMINAL_CWD")
}

// Local folder relative paths of this command line resolve against: the moved or handed folder, else the working directory
func localFolder() string {
	if folder := movedOrHandedFolder(); folder != "" {
		return folder
	}
	cwd, _ := os.Getwd()
	return cwd
}

// Folder a new session of this command line works in: the moved or handed folder, else the working folder setting of a backend other than local, empty for the launch folder
func sessionFolder(ctx context.Context) string {
	if folder := movedOrHandedFolder(); folder != "" {
		return folder
	}
	if backend := strings.TrimSpace(settings.WhereCommandsRun.Get(ctx)); backend == "" || backend == "local" {
		return ""
	}
	if cwd := strings.TrimSpace(settings.WorkingFolder.Get(ctx)); !cwdStandIns[cwd] {
		return cwd
	}
	return ""
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
	s = strings.TrimSpace(s)
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
