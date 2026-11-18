package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Exit status a launch step ends the process with, the sys.exit of the launcher
type tuiExitError struct{ code int }

// Message naming the exit status
func (e *tuiExitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// Session id the TUI recorded in its active session file, empty when unreadable or blank
func readTUIActiveSessionFile(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return ""
	}
	data, err := pyjson.Loads(strings.TrimPrefix(text, "\ufeff"))
	if err != nil {
		return ""
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return ""
	}
	value := obj.GetOr("session_id", nil)
	if !pyTruthyValue(value) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(value))
}

// Python truthiness of a decoded JSON value
func pyTruthyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

// Stripped non empty parts of comma separated items
func splitCommaItems(items []string) []string {
	var out []string
	for _, item := range items {
		for _, part := range strings.Split(item, ",") {
			if part = pystr.Strip(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// Toolsets of a comma separated flag as a clean list
func normalizeTUIToolsets(toolsets string) []string {
	if toolsets == "" {
		return nil
	}
	return splitCommaItems([]string{toolsets})
}

// Working directory for the TUI child, the PWD hint or the project root when the cwd was deleted
func safeTUICwd(env map[string]string) (string, error) {
	cwd, err := pypath.Getcwd()
	if err == nil {
		return cwd, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	candidate := env["PWD"]
	if candidate == "" {
		candidate = os.Getenv("PWD")
	}
	candidate = pystr.Strip(candidate)
	if info, serr := os.Stat(candidate); candidate != "" && serr == nil && info.IsDir() {
		return candidate, nil
	}
	return config.GetProjectRoot(), nil
}

// Creates the worktree checkout of a TUI launch after pruning stale trees, exit status 1 on failure
func setupTUIWorktree(ctx context.Context, stderr io.Writer) (map[string]any, error) {
	var info map[string]string
	err := func() error {
		if repo := tools.GitRepoRoot(); repo != "" {
			if err := tools.PruneStaleWorktrees(ctx, repo, 24); err != nil {
				return err
			}
			go tools.MaintainPackHealth(context.WithoutCancel(ctx), repo)
		}
		retainDB := home.StatePath(home.Dir(ctx))
		info = tools.SetupWorktree(ctx, "", true, "", &retainDB)
		return nil
	}()
	if err != nil {
		fmt.Fprintf(stderr, "✗ Failed to create TUI worktree: %v\n", err)
	}
	if len(info) == 0 {
		return nil, &tuiExitError{1}
	}
	out := make(map[string]any, len(info))
	for k, v := range info {
		out[k] = v
	}
	return out, nil
}

// Worktree info with its string values, the shape the worktree cleanup reads
func worktreeInfoStrings(info map[string]any) map[string]string {
	out := make(map[string]string, len(info))
	for k, v := range info {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Removes a clean idle TUI worktree on quit, true when the tree is gone
func removeIdleCleanTUIWorktree(ctx context.Context, info map[string]any) bool {
	path, _ := info["path"].(string)
	if path == "" {
		return false
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return false
	}
	dbPath := home.StatePath(home.Dir(ctx))
	if tools.RetainedTreeIsLive(dbPath, path, true) {
		return false
	}
	if tools.WorktreeStillInUse(path, 5*time.Second) {
		return false
	}
	if tools.TreeStillServed(dbPath, path) != "" {
		return false
	}
	cleanupWorktree(ctx, worktreeInfoStrings(info))
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// Refuses a safe mode or ignore user config launch with no model before the TUI starts, resumes excepted
func requireBypassModel(env map[string]string, model, resumeSessionID string, stderr io.Writer) error {
	bypass := false
	for _, name := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		if value, ok := env[name]; ok && provider.IsTruthyValue(value) {
			bypass = true
		}
	}
	if bypass && resumeSessionID == "" && model == "" && pystr.Strip(env["ICEBATH_MODEL"]) == "" {
		fmt.Fprintln(stderr, "Error: --safe-mode / --ignore-user-config (or ICEBATH_IGNORE_USER_CONFIG=1) read no profile "+
			"default model: pass --model explicitly.\n"+
			"  Example: icebath --safe-mode --provider openrouter --model anthropic/claude-sonnet-4")
		return &tuiExitError{1}
	}
	return nil
}

// Seams of a TUI launch, the worktree setup, the terminal UI run and relaunch
type tuiLaunchDeps struct {
	setupWorktree func(ctx context.Context) (map[string]any, error)
	run           func(ctx context.Context, env map[string]string) (int, error)
	relaunch      func(args []string, preserveInherited bool)
	stdout        io.Writer
	stderr        io.Writer
}

// Exit status of a launch error, the error printed when it is not a requested exit
func tuiLaunchExitCode(err error, stderr io.Writer) int {
	var exit *tuiExitError
	if errors.As(err, &exit) {
		return exit.code
	}
	fmt.Fprintf(stderr, "%v\n", err)
	return 1
}

// Runs the terminal UI with the launch flags in its environment, returning the process exit status
func launchTUIWith(ctx context.Context, l TUILaunch, deps tuiLaunchDeps) int {
	env, err := tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: false})
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	if err := requireBypassModel(env, l.Model, l.Resume, deps.stderr); err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	cwd, err := safeTUICwd(env)
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	env["ICEBATH_CWD"] = cwd
	if err := configureTUIAttachment(ctx, env, l.Resume, ""); err != nil {
		fmt.Fprintf(deps.stderr, "Error: %v\n", err)
		return 1
	}
	if err := config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{Env: env}); err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to apply terminal config bridge for TUI launch: %v", err))
	}
	tmpDir, err := pypath.Gettempdir()
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	activeFile, err := os.CreateTemp(tmpDir, "icebath-tui-active-session-*.json")
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	activeSessionFile := activeFile.Name()
	_ = activeFile.Close()
	env["ICEBATH_TUI_ACTIVE_SESSION_FILE"] = activeSessionFile
	nativeMode := l.Native
	if !nativeMode {
		if cfg, cerr := config.Load(home.Dir(ctx), nil); cerr == nil {
			if display, ok := cfg["display"].(map[string]any); ok {
				nativeMode = provider.IsTruthyValue(display["tui_native"])
			}
		}
	}
	env["ICEBATH_TUI_NATIVE"] = "0"
	if nativeMode {
		env["ICEBATH_TUI_NATIVE"] = "1"
	}
	var wtInfo map[string]any
	if l.Worktree {
		if wtInfo, err = deps.setupWorktree(ctx); err != nil {
			return tuiLaunchExitCode(err, deps.stderr)
		}
		path, _ := wtInfo["path"].(string)
		env["ICEBATH_CWD"] = path
		env["TERMINAL_CWD"] = path
	}
	terminalEnv := env["TERMINAL_ENV"]
	if terminalEnv == "" {
		terminalEnv = "local"
	}
	if terminalEnv == "local" {
		env["TERMINAL_CWD"] = env["ICEBATH_CWD"]
		env["ICEBATH_TUI_CWD"] = env["ICEBATH_CWD"]
	} else {
		delete(env, "ICEBATH_TUI_CWD")
	}
	clientEnv, err := gatewaycli.ShellClientEnv(ctx)
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	clientEnvJSON, err := pyjson.Dumps(clientEnv, pyjson.SortKeys(false))
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	env["ICEBATH_TUI_CLIENT_ENV"] = clientEnvJSON
	skillsValue := strings.Join(splitCommaItems(l.Skills), ",")
	maxTurns := ""
	if l.MaxTurns != nil {
		maxTurns = strconv.Itoa(*l.MaxTurns)
	}
	toolProgress := ""
	if l.Verbose {
		toolProgress = "verbose"
	} else if l.Quiet {
		toolProgress = "off"
	}
	flag := func(on bool) string {
		if on {
			return "1"
		}
		return ""
	}
	for _, pair := range [][2]string{
		{"ICEBATH_MODEL", l.Model}, {"ICEBATH_INFERENCE_MODEL", l.Model},
		{"ICEBATH_TUI_PROVIDER", l.Provider}, {"ICEBATH_INFERENCE_PROVIDER", l.Provider},
		{"ICEBATH_TUI_TOOLSETS", strings.Join(normalizeTUIToolsets(l.Toolsets), ",")},
		{"ICEBATH_TUI_SKILLS", skillsValue},
		{"ICEBATH_TUI_BASE_URL", l.BaseURL}, {"ICEBATH_TUI_API_KEY", l.APIKey}, {"ICEBATH_TUI_REASONING", l.Reasoning},
		{"ICEBATH_TUI_CHECKPOINTS", flag(l.Checkpoints)},
		{"ICEBATH_TUI_PASS_SESSION_ID", flag(l.PassSessionID)},
		{"ICEBATH_TUI_MAX_TURNS", maxTurns},
		{"ICEBATH_TUI_TOOL_PROGRESS", toolProgress},
		{"ICEBATH_ACCEPT_HOOKS", flag(l.AcceptHooks)},
	} {
		if pair[1] != "" {
			env[pair[0]] = pair[1]
		}
	}
	delete(env, "ICEBATH_TUI_RESUME")
	if l.Resume != "" {
		env["ICEBATH_TUI_RESUME"] = l.Resume
	}
	code, callErr := deps.run(ctx, env)
	exited := callErr == nil && (code == 0 || code == 130)
	attachedSession := readTUIActiveSessionFile(activeSessionFile)
	if attachedSession == "" {
		attachedSession = l.Resume
	}
	_ = os.Remove(activeSessionFile)
	if wtInfo != nil && exited {
		tools.AskExitChoice(ctx, wtInfo)
	}
	if wtInfo != nil && tools.ApplyExitChoice(ctx, wtInfo, func(s string) { fmt.Fprintln(deps.stdout, s) }) {
		wtInfo = nil
	}
	if wtInfo != nil && removeIdleCleanTUIWorktree(ctx, wtInfo) {
		wtInfo = nil
	}
	if wtInfo != nil {
		path, _ := wtInfo["path"].(string)
		if attachedSession != "" {
			repoRoot, _ := wtInfo["repo_root"].(string)
			tools.RetainWorktreeForSession(repoRoot, path, attachedSession, home.StatePath(home.Dir(ctx)))
		}
		fmt.Fprintf(deps.stderr, "Worktree retained for this session: %s\n", path)
	}
	if callErr != nil {
		return tuiLaunchExitCode(callErr, deps.stderr)
	}
	if code == 42 {
		fmt.Fprint(deps.stdout, "\n☤ Launching update...\n\n")
		deps.relaunch([]string{"update"}, false)
	}
	return code
}

// Runs the Go terminal UI in this process with the launch environment as the process environment
func runTerminalUI(ctx context.Context, env map[string]string) (int, error) {
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if _, kept := env[name]; !kept {
				if err := os.Unsetenv(name); err != nil {
					return 0, err
				}
			}
		}
	}
	for name, value := range env {
		if err := os.Setenv(name, value); err != nil {
			return 0, err
		}
	}
	return tui.Run(ctx, terminalUIHost()), nil
}
