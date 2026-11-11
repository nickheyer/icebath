package builtin

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	impeccableHookCommandMarker = "skills/impeccable/scripts/impeccable"

	hookTimeoutSeconds     = 5
	hookStatusMessage      = "Checking UI changes"
	hookStopTimeoutSeconds = 30
	hookStopStatusMessage  = "Design deep pass"

	hookLauncherName        = "impeccable"
	hookLauncherNameWindows = "impeccable.cmd"

	claudeProjectHookScripts = "${CLAUDE_PROJECT_DIR}/.claude/skills/impeccable/scripts"
	claudePluginHookScripts  = "${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts"
	codexPluginHookScripts   = "${PLUGIN_ROOT}/skills/impeccable/scripts"
	cursorHookScripts        = ".cursor/skills/impeccable/scripts"
	githubProjectHookScripts = "$(git rev-parse --show-toplevel)/.github/skills/impeccable/scripts"
	grokProjectHookScripts   = ".grok/skills/impeccable/scripts"
)

// Options of the Claude compatible hook set, the Windows sibling key, session start entry, guard and env
type claudeHooksOptions struct {
	WindowsKey      string
	SessionIdentity bool
	Guard           func(launcherPath, verb string) string
	Env             *pyjson.Object
}

// Launcher invocation that exits zero when the launcher file is missing
func guardedLauncher(launcherPath, verb string) string {
	return `[ ! -f "` + launcherPath + `" ] || "` + launcherPath + `" ` + verb
}

// Guarded launcher invocation run through sh for hosts that drop the executable bit
func guardedLauncherViaSh(launcherPath, verb string) string {
	return `[ ! -f "` + launcherPath + `" ] || sh "` + launcherPath + `" ` + verb
}

// Windows command for harnesses reading commandWindows, quoted backslash path behind cmd /c
func windowsLauncherCommand(launcherCmdPath, verb string) string {
	quoted := `"` + strings.ReplaceAll(launcherCmdPath, "/", `\`) + `"`
	return "cmd /c if exist " + quoted + " " + quoted + " " + verb
}

// Windows command for the VS Code windows sibling, bare backslash path behind cmd /c
func vscodeWindowsLauncherCommand(launcherCmdPath, verb string) string {
	bare := strings.ReplaceAll(launcherCmdPath, "/", `\`)
	return "cmd /c if exist " + bare + " " + bare + " " + verb
}

var hookWindowsForms = map[string]func(launcherCmdPath, verb string) string{
	"commandWindows": windowsLauncherCommand,
	"windows":        vscodeWindowsLauncherCommand,
}

// PostToolUse and Stop entries, with SessionStart when asked, in the Claude hook schema
func buildClaudeCompatibleHooks(matcher, scriptsDir string, opts claudeHooksOptions) *pyjson.Object {
	guard := opts.Guard
	if guard == nil {
		guard = guardedLauncher
	}
	command := guard(scriptsDir+"/"+hookLauncherName, "hook")
	handler := func(timeout int, status string) *pyjson.Object {
		h := pyjson.ObjectOf("type", "command", "command", command)
		if opts.WindowsKey != "" {
			h.Set(opts.WindowsKey, hookWindowsForms[opts.WindowsKey](scriptsDir+"/"+hookLauncherNameWindows, "hook"))
		}
		if opts.Env != nil {
			h.Set("env", opts.Env)
		}
		h.Set("timeout", timeout)
		h.Set("statusMessage", status)
		return h
	}
	hooks := pyjson.NewObject()
	if opts.SessionIdentity {
		hooks.Set("SessionStart", []any{pyjson.ObjectOf("hooks", []any{handler(hookTimeoutSeconds, "Preparing build session")})})
	}
	hooks.Set("PostToolUse", []any{pyjson.ObjectOf("matcher", matcher, "hooks", []any{handler(hookTimeoutSeconds, hookStatusMessage)})})
	hooks.Set("Stop", []any{pyjson.ObjectOf("hooks", []any{handler(hookStopTimeoutSeconds, hookStopStatusMessage)})})
	return hooks
}

// Project settings manifest of the Claude Code build
func buildClaudeSettingsManifest() *pyjson.Object {
	return pyjson.ObjectOf(
		"description", "Impeccable design detector: immediate-tier checks after Edit/Write on UI files, full-rule deep pass on Stop.",
		"hooks", buildClaudeCompatibleHooks("Edit|Write", claudeProjectHookScripts, claudeHooksOptions{SessionIdentity: true}))
}

// hooks/hooks.json of the Claude plugin subtree, resolved against the plugin root
func buildClaudePluginHooksManifest() *pyjson.Object {
	return pyjson.ObjectOf("hooks", buildClaudeCompatibleHooks("Edit|Write", claudePluginHookScripts, claudeHooksOptions{
		SessionIdentity: true,
		WindowsKey:      "windows",
		Guard:           guardedLauncherViaSh,
		Env:             pyjson.ObjectOf("IMPECCABLE_HOOK_HARNESS", "vscode"),
	}))
}

// hooks/hooks.json of the OpenAI plugin bundle, resolved against PLUGIN_ROOT
func buildCodexPluginHooksManifest() *pyjson.Object {
	return pyjson.ObjectOf("hooks", buildClaudeCompatibleHooks("Edit|Write|apply_patch", codexPluginHookScripts, claudeHooksOptions{WindowsKey: "commandWindows"}))
}

// Codex project hooks pointing at the skill payload of the given install directory
func buildCodexHooksManifest(skillDir string) *pyjson.Object {
	return pyjson.ObjectOf("hooks", buildClaudeCompatibleHooks("Edit|Write|apply_patch", skillDir+"/skills/impeccable/scripts", claudeHooksOptions{WindowsKey: "commandWindows"}))
}

// Cursor postToolUse hook on the Write tool
func buildCursorHooksManifest(scriptsDir string) *pyjson.Object {
	return pyjson.ObjectOf("version", 1, "hooks", pyjson.ObjectOf("postToolUse", []any{pyjson.ObjectOf(
		"command", guardedLauncher(scriptsDir+"/"+hookLauncherName, "hook"),
		"matcher", "^Write$",
		"timeout", hookTimeoutSeconds)}))
}

// GitHub Copilot postToolUse hook in its flat bash and timeoutSec schema
func buildGitHubHooksManifest() *pyjson.Object {
	return pyjson.ObjectOf("version", 1, "hooks", pyjson.ObjectOf("postToolUse", []any{pyjson.ObjectOf(
		"type", "command",
		"matcher", "edit|create|apply_patch",
		"bash", guardedLauncher(githubProjectHookScripts+"/"+hookLauncherName, "hook"),
		"timeoutSec", hookTimeoutSeconds)}))
}

// Grok Build project hooks in the Claude schema
func buildGrokHooksManifest() *pyjson.Object {
	return pyjson.ObjectOf("hooks", buildClaudeCompatibleHooks("Edit|Write|MultiEdit", grokProjectHookScripts, claudeHooksOptions{}))
}

// Gemini BeforeTool and AfterAgent hooks with millisecond timeouts and a bare project path
func buildGeminiHooksManifest() *pyjson.Object {
	launcher := "$GEMINI_PROJECT_DIR/.gemini/skills/impeccable/scripts/impeccable"
	command := "[ ! -f " + launcher + " ] || " + launcher + " hook"
	return pyjson.ObjectOf("hooks", pyjson.ObjectOf(
		"BeforeTool", []any{pyjson.ObjectOf("matcher", "^run_shell_command$", "hooks", []any{pyjson.ObjectOf(
			"name", "impeccable-session", "type", "command", "command", command, "timeout", 5000)})},
		"AfterAgent", []any{pyjson.ObjectOf("hooks", []any{pyjson.ObjectOf(
			"name", "impeccable-completion", "type", "command", "command", command, "timeout", 30000)})}))
}

// Hook manifest of a provider hook family, nil for a family without one
func hooksJSONFor(provider, configDir string) *pyjson.Object {
	switch provider {
	case "claude":
		return buildClaudeSettingsManifest()
	case "codex":
		if configDir == "" {
			configDir = ".codex"
		}
		return buildCodexHooksManifest(configDir)
	case "cursor":
		return buildCursorHooksManifest(cursorHookScripts)
	case "github":
		return buildGitHubHooksManifest()
	case "grok":
		return buildGrokHooksManifest()
	case "gemini":
		return buildGeminiHooksManifest()
	}
	return nil
}
