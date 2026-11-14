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
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
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

// Prefix length of a canonical local session id the epilogue prints
const tuiShortResumeLen = 22

// Unique 22 character prefix of a canonical local id, the whole id otherwise or when the prefix is ambiguous
func shortResumeID(db *state.SessionDB, sessionID string) string {
	if !strings.HasPrefix(sessionID, "local-") || len(sessionID) <= tuiShortResumeLen {
		return sessionID
	}
	short := sessionID[:tuiShortResumeLen]
	resolved, err := db.ResolveSessionID(short)
	if err != nil || resolved != sessionID {
		return sessionID
	}
	return short
}

// Session row followed by each branch ancestor it was cut from
func branchLineage(ctx context.Context, db *state.SessionDB, session *pyjson.Object) ([]*pyjson.Object, error) {
	rows := []*pyjson.Object{session}
	seen := map[string]bool{pyrepr.Str(session.GetOr("id", nil)): true}
	for {
		raw := rows[len(rows)-1].GetOr("model_config", nil)
		if !pyTruthyValue(raw) {
			raw = "{}"
		}
		text, ok := raw.(string)
		if !ok {
			break
		}
		cfg, err := pyjson.Loads(text)
		if err != nil {
			break
		}
		obj, ok := cfg.(*pyjson.Object)
		if !ok {
			break
		}
		parentValue := obj.GetOr("_branched_from", nil)
		if !pyTruthyValue(parentValue) {
			break
		}
		switch parentValue.(type) {
		case []any, *pyjson.Object:
			return nil, fmt.Errorf("Error binding parameter 1: type '%s' is not supported", pyrepr.TypeName(parentValue))
		}
		parentID := pyrepr.Str(parentValue)
		if seen[parentID] {
			break
		}
		parent, err := db.GetSession(ctx, parentID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			break
		}
		seen[parentID] = true
		rows = append(rows, parent)
	}
	return rows, nil
}

// Integer of a row count column, zero for falsy values
func rowCount(v any) (int64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseInt(pystr.Strip(x), 10, 64)
	}
	return 0, fmt.Errorf("int() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))
}

// Prints the resume commands, title, message count and lineage token totals of the session the TUI left
func printTUIExitSummary(ctx context.Context, w io.Writer, sessionID, activeSessionFile string) {
	target := readTUIActiveSessionFile(activeSessionFile)
	if target == "" {
		target = sessionID
	}
	if target == "" {
		target = resolveLastSession(ctx, "tui")
	}
	if target == "" {
		return
	}
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		return
	}
	title, resumeID, messageCount, tokens, ok := func() (string, string, int64, map[string]int64, bool) {
		defer db.Close()
		session, err := db.GetSession(ctx, target)
		if err != nil || session == nil {
			return "", "", 0, nil, false
		}
		title, _, err := db.GetSessionTitle(ctx, target)
		if err != nil {
			return "", "", 0, nil, false
		}
		resumeID := shortResumeID(db, target)
		messageCount, err := rowCount(session.GetOr("message_count", nil))
		if err != nil || messageCount == 0 {
			return "", "", 0, nil, false
		}
		tokens := map[string]int64{}
		lineage, err := branchLineage(ctx, db, session)
		if err != nil {
			return "", "", 0, nil, false
		}
		for _, row := range lineage {
			for _, k := range []string{"input", "output", "cache_read", "cache_write", "reasoning"} {
				n, err := rowCount(row.GetOr(k+"_tokens", nil))
				if err != nil {
					return "", "", 0, nil, false
				}
				tokens[k] += n
			}
		}
		return title, resumeID, messageCount, tokens, true
	}()
	if !ok {
		return
	}
	profileFlag := ""
	if active := profiles.GetActiveProfileName(ctx); active != "default" && active != "custom" {
		profileFlag = " -p " + active
	}
	fmt.Fprintf(w, "\n%s\n  icebath --tui --resume %s%s\n", i18n.T(ctx, "cli.session.exit_resume_hint", "", nil), resumeID, profileFlag)
	if title != "" {
		fmt.Fprintf(w, "  icebath --tui -c \"%s\"%s\n", title, profileFlag)
	}
	fmt.Fprintf(w, "\nSession:        %s\n", resumeID)
	if title != "" {
		fmt.Fprintf(w, "Title:          %s\n", title)
	}
	fmt.Fprintf(w, "Messages:       %d\n", messageCount)
	total := tokens["input"] + tokens["output"] + tokens["cache_read"] + tokens["cache_write"] + tokens["reasoning"]
	fmt.Fprintf(w, "Tokens:         %d (in %d, out %d, cache %d, reasoning %d)\n",
		total, tokens["input"], tokens["output"], tokens["cache_read"]+tokens["cache_write"], tokens["reasoning"])
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

// Worktree info with its string values, the shape the classic cleanup reads
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
			"  Example: icebath --tui --safe-mode --provider openrouter --model anthropic/claude-sonnet-4")
		return &tuiExitError{1}
	}
	return nil
}

// Seams of a TUI launch, the worktree setup, the terminal UI run, epilogue and relaunch
type tuiLaunchDeps struct {
	setupWorktree func(ctx context.Context) (map[string]any, error)
	run           func(ctx context.Context, env map[string]string) (int, error)
	printSummary  func(ctx context.Context, sessionID, activeSessionFile string)
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
	if err := configureTUIAttachment(env, l.Resume, ""); err != nil {
		fmt.Fprintf(deps.stderr, "Error: %v\n", err)
		return 1
	}
	if err := config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{Env: env}); err != nil {
		cliLogger.Debug(fmt.Sprintf("Failed to apply terminal config bridge for TUI launch: %v", err))
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
		{"ICEBATH_TUI_QUERY", l.Query}, {"ICEBATH_TUI_IMAGE", l.Image},
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
	if exited {
		deps.printSummary(ctx, l.Resume, activeSessionFile)
	}
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
	return tui.Run(ctx), nil
}
