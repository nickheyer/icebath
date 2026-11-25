package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tui"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
	text, err := textutil.DecodeUTF8(raw)
	if err != nil {
		return ""
	}
	data, err := jsonutil.Decode(strings.TrimPrefix(text, "\ufeff"))
	if err != nil {
		return ""
	}
	obj, ok := data.(*jsonutil.Object)
	if !ok {
		return ""
	}
	value := obj.Value("session_id")
	if !anyutil.IsTrue(value) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(value))
}

// Stripped non empty parts of comma separated items
func splitCommaItems(items []string) []string {
	var out []string
	for _, item := range items {
		for _, part := range strings.Split(item, ",") {
			if part = strings.TrimSpace(part); part != "" {
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
	cwd, err := os.Getwd()
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
	candidate = strings.TrimSpace(candidate)
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
	if bypass && resumeSessionID == "" && model == "" && strings.TrimSpace(env["ICEBATH_MODEL"]) == "" {
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
	relaunch      func(args []string, preserveInherited bool, env []string)
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
	tmpDir, err := pathutil.TempDir()
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
	nativeMode := l.Native || settings.DisplayTuiNative.Get(ctx)
	env["ICEBATH_TUI_NATIVE"] = "0"
	if nativeMode {
		env["ICEBATH_TUI_NATIVE"] = "1"
	}
	// The window paints its first frame before the background service answers, so the mouse mode of that frame travels with the launch
	env["ICEBATH_TUI_MOUSE"] = settings.Mouse.Get(ctx)
	var wtInfo map[string]any
	if l.Worktree {
		if wtInfo, err = deps.setupWorktree(ctx); err != nil {
			return tuiLaunchExitCode(err, deps.stderr)
		}
		path, _ := wtInfo["path"].(string)
		env["ICEBATH_CWD"] = path
		env["TERMINAL_CWD"] = path
	}
	if settings.WhereCommandsRun.Get(ctx) == "local" {
		env["TERMINAL_CWD"] = env["ICEBATH_CWD"]
		env["ICEBATH_TUI_CWD"] = env["ICEBATH_CWD"]
	} else {
		delete(env, "ICEBATH_TUI_CWD")
	}
	clientEnv, err := gatewaycli.ShellClientEnv(ctx)
	if err != nil {
		return tuiLaunchExitCode(err, deps.stderr)
	}
	clientEnvJSON, err := jsonutil.Encode(clientEnv, jsonutil.Spaced(), jsonutil.ASCII())
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
	queued, err := tui.TakeAfterExit()
	if err != nil {
		fmt.Fprintf(deps.stderr, "icebath: could not read the commands queued to run after exit: %v\n", err)
	}
	if code != 0 {
		printNotRun(deps.stderr, "icebath did not close normally, so these commands did not run:", queued)
		return code
	}
	if len(queued) == 0 {
		return code
	}
	if reopen, env := runAfterExit(deps, queued); reopen {
		fmt.Fprint(deps.stdout, "\n󰆧 Opening icebath again...\n\n")
		deps.relaunch(nil, false, env)
	}
	return code
}

// Lists queued commands that did not run, each as a line to run by hand
func printNotRun(w io.Writer, why string, queued [][]string) {
	if len(queued) == 0 {
		return
	}
	fmt.Fprintln(w, why)
	for _, argv := range queued {
		fmt.Fprintln(w, "  icebath "+shellutil.Join(argv))
	}
}

// Runs the queued commands in this terminal, the background service stopped first when one needs it, reporting whether icebath opens again and the environment it opens with (this process's when nil)
func runAfterExit(deps tuiLaunchDeps, queued [][]string) (bool, []string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(deps.stderr, "icebath: %v\n", err)
		printNotRun(deps.stderr, "These commands did not run:", queued)
		return false, nil
	}
	var env []string
	run := func(argv []string) int {
		line := "icebath " + shellutil.Join(argv)
		fmt.Fprintf(deps.stdout, "\n󰆧 %s\n\n", line)
		code, err := runAndWait(append([]string{exe}, argv...), env)
		if err != nil {
			fmt.Fprintf(deps.stderr, "%s: %v\n", line, err)
		}
		return code
	}
	if slices.ContainsFunc(queued, stopsServiceFirst) {
		run([]string{"gateway", "stop"})
	}
	reopen := true
	for i, argv := range queued {
		code := run(argv)
		if code == 130 {
			printNotRun(deps.stderr, "Stopped. These commands did not run:", queued[i+1:])
			return false, nil
		}
		switch path := strings.Join(argv[:min(2, len(argv))], " "); {
		case argv[0] == "uninstall", path == "gateway stop":
			reopen = false
		case path == "profile use" && code == 0:
			// The profile this process was started on would otherwise win over the new sticky one in what runs next
			env = environWith(home.EnvHome, home.Root())
		}
	}
	return reopen, env
}

// This process's environment with one variable set to value, the name matched without case on Windows
func environWith(name, value string) []string {
	out := []string{name + "=" + value}
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == name || (runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Whether a queued command needs the background service stopped first, one that removes icebath, replaces the service or repairs sessions
func stopsServiceFirst(argv []string) bool {
	switch argv[0] {
	case "uninstall", "sessions":
		return true
	case "gateway":
		return len(argv) < 2 || argv[1] != "stop"
	}
	return false
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
	return tui.Run(ctx, terminalUIHost(ctx)), nil
}
