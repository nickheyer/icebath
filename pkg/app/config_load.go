package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
	path := pathutil.ExpandUser(filePath)
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
	data, err := jsonutil.Decode(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %s", path, err))
		return []any{}
	}
	list, ok := jsonutil.Plain(data).([]any)
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

// Text of a value, empty for nil and for false, zero and empty values
func strOrEmpty(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}

// Parses a reasoning effort level, warning once when a non empty value is unknown
func parseReasoningConfig(effort any) map[string]any {
	result := config.ParseReasoningEffort(effort)
	if anyutil.IsTrue(effort) && strings.TrimSpace(anyutil.String(effort)) != "" && result == nil {
		pkgLog().Warn(fmt.Sprintf("Unknown reasoning_effort '%s', using default (medium)", anyutil.String(effort)))
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
