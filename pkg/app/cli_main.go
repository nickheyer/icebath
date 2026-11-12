package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// MainOptions carries the cli.py main keyword arguments
type MainOptions struct {
	Model, Reasoning, Toolsets, Query, Q, OutputFormat, Provider, Image, Resume, APIKey, BaseURL             string
	Oneshot, IgnoreRules, IgnoreUserConfig, Compact, Verbose, Quiet, Worktree, W, Checkpoints, PassSessionID bool
	ListTools, ListToolsets, Gateway                                                                         bool
	RunBudget                                                                                                float64
	Skills                                                                                                   []string
	MaxTurns                                                                                                 *int
}

// ErrSetupCancelled reports a provider picker the user left with Ctrl+C, EOF or an exit
var ErrSetupCancelled = errors.New("provider setup cancelled")

// Seam over the shared provider picker, the hermes_cli.main.select_provider_and_model the first run tests patch
var firstRunProviderPicker = provider.SelectProviderAndModel

// Runs the provider picker with no namespace, a Ctrl-C, EOF or setup exit reported as ErrSetupCancelled
func selectProviderAndModel(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(provider.SetupExit); !ok {
				panic(r)
			}
			err = ErrSetupCancelled
		}
	}()
	err = firstRunProviderPicker(ctx, nil)
	if provider.IsPromptInterrupt(err) {
		return ErrSetupCancelled
	}
	return err
}

// Interactive chat entry, a single query when one is given, returning the exit code and the error it raises
func Main(ctx context.Context, o MainOptions) (code int, err error) {
	defer runAtExit()
	ConfigureWindowsStdio()
	_ = os.Setenv("ICEBATH_INTERACTIVE", "1")
	if o.Gateway {
		return runLegacyGateway(ctx), nil
	}
	defer func() {
		if r := recover(); r != nil {
			if _, interrupted := r.(keyboardInterrupt); interrupted {
				fmt.Fprintln(stderrWriter(), "KeyboardInterrupt")
				code = 130
				return
			}
			if raised, isErr := r.(error); isErr && isValueErrorClass(raised) {
				code, err = 1, raised
				return
			}
			exit, ok := r.(singleQueryExit)
			if !ok {
				panic(r)
			}
			code = exit.code
		}
	}()
	if !(o.ListTools || o.ListToolsets) {
		tools.RegisterSelf(ctx, "cli", "", nil)
		if warning := profiles.SharedProfileWarning(ctx, home.Dir(ctx), ""); warning != "" {
			fmt.Fprintln(stderrWriter(), i18n.T(ctx, "cli.startup.warning", "", map[string]any{"warning": warning}))
		}
	}
	if c := console(); c.StdinIsTTY() && c.StdoutIsTTY() {
		detectLightMode()
	}
	joinWorktree := startWorktreeSetup(ctx, o.ListTools, o.ListToolsets, o.Worktree, o.W)
	query := o.Query
	if query == "" {
		query = o.Q
	}
	quiet := o.Quiet
	if o.OutputFormat == "stream-json" {
		if query == "" {
			return 1, &appValueError{msg: "--format stream-json requires -q/--query"}
		}
		quiet = true
	}
	maxTurns := o.MaxTurns
	var runBudget *float64
	if o.RunBudget > 0 {
		v := o.RunBudget
		runBudget = &v
	}
	compact := o.Compact
	c, err := buildCLIFromArgs(ctx, CLIOptions{Model: o.Model, Toolsets: splitToolsetsArg(o.Toolsets), Provider: o.Provider, Reasoning: o.Reasoning,
		APIKey: o.APIKey, BaseURL: o.BaseURL, MaxTurns: maxTurns, RunBudget: runBudget, Verbose: o.Verbose, Compact: &compact, Resume: o.Resume,
		Checkpoints: o.Checkpoints, PassSessionID: o.PassSessionID, IgnoreRules: o.IgnoreRules}, o.Toolsets != "", o.Skills)
	if err != nil {
		if home.EmitPartialUpdateHint(err, stderrWriter()) {
			return 1, nil
		}
		return 1, err
	}
	if joinWorktree != nil {
		if wtInfo := joinWorktree(); wtInfo == nil {
			return 0, nil
		}
	}
	if o.ListTools || o.ListToolsets {
		c.showBanner()
		if o.ListTools {
			c.showTools()
		} else {
			c.showToolsets()
		}
		return 0, nil
	}
	defer runCleanup(ctx, c.env, true)
	installSingleQuerySignalHandlers(c)
	defer c.signalStop()
	if query != "" || o.Image != "" {
		runSingleQueryMode(c, query, o.Image, quiet, o.Oneshot, o.OutputFormat == "stream-json")
		return 0, nil
	}
	c.run()
	return 0, nil
}

// Runs main as the chat command does, a ValueError printed to stdout as Error then its message with exit 1
func ChatMain(ctx context.Context, o MainOptions) int {
	code, err := Main(ctx, o)
	if err == nil {
		return code
	}
	if isValueErrorClass(err) {
		fmt.Fprintln(console().Out, "Error: "+err.Error())
		return 1
	}
	fmt.Fprintln(stderrWriter(), err.Error())
	return 1
}

// Reports an error of the Python ValueError class or a subclass, the errors an except ValueError clause catches
func isValueErrorClass(err error) bool {
	mro := pyrepr.BuiltinExceptionMRO(pyrepr.ExceptionName(err))
	var declared interface{ PyExceptionMRO() []string }
	if errors.As(err, &declared) {
		mro = declared.PyExceptionMRO()
	}
	return slices.Contains(mro, "ValueError")
}

// Splits a comma separated toolsets flag, nil when the flag was not given
func splitToolsetsArg(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		out = append(out, strings.TrimSpace(t))
	}
	return out
}

// NewCLI builds the classic CLI with CLI args winning over config and resume restoring history from SQLite
func NewCLI(ctx context.Context, o CLIOptions) (*CLI, error) {
	ctx = tools.WithThreadLocals(ctx)
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		cwd, _ := os.Getwd()
		env = &tools.Env{Home: home.Dir(ctx), Cwd: cwd}
		ctx = tools.WithToolEnv(ctx, env)
	}
	turnCtx, cancel := context.WithCancelCause(ctx)
	c := &CLI{ctx: ctx, env: env, turnCtx: turnCtx, turnCancel: func() { cancel(tools.ErrKeyboardInterrupt) }, signalStop: func() {}}
	c.initDisplayOptions(o.Verbose, o.Compact)
	if err := c.initModelRouting(o); err != nil {
		return nil, err
	}
	c.initRuntimeState(o.Resume)
	return c, nil
}

// Resolves the toolset list, constructs the CLI and starts the background skills preload
func buildCLIFromArgs(ctx context.Context, o CLIOptions, typedToolsets bool, skillsArg []string) (*CLI, error) {
	toolsetsList := o.Toolsets
	if !typedToolsets {
		env := tools.ToolEnvFrom(ctx)
		cwd, _ := os.Getwd()
		if env != nil && env.Cwd != "" {
			cwd = env.Cwd
		}
		toolsetsList = tools.CodingSelection(env, "cli", cwd)
		o.Toolsets = toolsetsList
	}
	parsedSkills := parseSkillsArgument(skillsArg)
	c, err := NewCLI(ctx, o)
	if err != nil {
		return nil, err
	}
	if !typedToolsets {
		c.explicitToolsets = false
		c.typedToolsets = nil
	} else {
		c.typedToolsets = toolsetsList
	}
	var autoLoadNames []string
	if !c.ignoreRules {
		autoLoadNames = skillsScope(c.ctx).ResolveAutoLoadSkills(c.config)
	}
	if len(autoLoadNames) == 0 {
		c.autoLoadSkillsResult = &skillsPromptResult{Prompt: "", Loaded: []string{}, Missing: []string{}}
	}
	if len(parsedSkills) > 0 || len(autoLoadNames) > 0 {
		requested := append([]string{}, autoLoadNames...)
		for _, s := range parsedSkills {
			if !containsString(autoLoadNames, s) {
				requested = append(requested, s)
			}
		}
		c.preloadSkillsRequested = requested
		done := make(chan struct{})
		c.preloadSkillsDone = done
		scope := skillsScope(c.ctx)
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					c.preloadSkillsError = fmt.Errorf("%v", r)
				}
			}()
			if len(autoLoadNames) > 0 {
				prompt, loaded, missing := scope.BuildAutoLoadPrompt(c.ctx, c.sessionID, c.config)
				c.autoLoadSkillsResult = &skillsPromptResult{Prompt: prompt, Loaded: loaded, Missing: missing}
			}
			if len(parsedSkills) > 0 {
				excluded := map[string]bool{}
				for _, name := range c.autoLoadSkillsResult.Loaded {
					excluded[name] = true
				}
				prompt, loaded, missing := scope.BuildPreloadedSkillsPrompt(c.ctx, parsedSkills, c.sessionID, excluded)
				c.preloadSkillsResult = &skillsPromptResult{Prompt: prompt, Loaded: loaded, Missing: missing}
			}
		}()
	}
	return c, nil
}

// Joins the background skills preload and folds it into the prompt, failing only when every requested skill was unknown
func (c *CLI) finalizePreloadedSkills() error {
	if c.preloadSkillsFinalized {
		return nil
	}
	if c.preloadSkillsDone == nil {
		c.preloadSkillsFinalized = true
		return nil
	}
	select {
	case <-c.preloadSkillsDone:
	case <-time.After(120 * time.Second):
	}
	c.preloadSkillsFinalized = true
	if c.preloadSkillsError != nil {
		return c.preloadSkillsError
	}
	auto := c.autoLoadSkillsResult
	if auto != nil && len(auto.Missing) > 0 {
		cliLogger.Warn(fmt.Sprintf("skills.auto_load: skill(s) not found or disabled, skipped: %s", strings.Join(auto.Missing, ", ")))
	}
	c.preloadedSkills = []string{}
	if auto != nil {
		c.preloadedSkills = append(c.preloadedSkills, auto.Loaded...)
	}
	result := c.preloadSkillsResult
	if result == nil {
		return nil
	}
	if len(result.Missing) > 0 {
		if len(result.Loaded) == 0 {
			return errors.New(skills.FormatMissingSkills(result.Missing))
		}
		cliLogger.Warn(fmt.Sprintf("Skipping %s. Continuing with: %s. List available skills with `icebath skills list`.",
			skills.FormatMissingSkills(result.Missing), strings.Join(result.Loaded, ", ")))
	}
	if result.Prompt != "" {
		var parts []string
		for _, p := range []string{c.systemPrompt, result.Prompt} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		c.systemPrompt = strings.TrimSpace(strings.Join(parts, "\n\n"))
	}
	for _, name := range result.Loaded {
		if !containsString(c.preloadedSkills, name) {
			c.preloadedSkills = append(c.preloadedSkills, name)
		}
	}
	return nil
}

// Claims the global active session slot for this CLI process, printing the refusal when another holds it
func (c *CLI) claimActiveSession(surface string, stderr bool) bool {
	if c.activeSessionLease != nil {
		return true
	}
	lease, message, err := TryAcquireActiveSession(c.ctx, c.sessionID, surface, c.config, map[string]any{"live_session_id": c.sessionID})
	if err != nil {
		cliLogger.Warn(fmt.Sprintf("Failed to claim active session slot: %v", err))
		return true
	}
	if message != nil && message.Message != "" {
		if stderr {
			fmt.Fprintln(stderrWriter(), FormatRefusalStderr(message))
		} else {
			c.consolePrint(fmt.Sprintf("[bold red]%s[/]", message.Message))
		}
		return false
	}
	c.activeSessionLease = lease
	return true
}

// Releases the active session slot this process holds
func (c *CLI) releaseActiveSession() {
	lease := c.activeSessionLease
	if lease == nil {
		return
	}
	if err := lease.Release(); err != nil {
		cliLogger.Debug(fmt.Sprintf("Failed to release active session slot: %v", err))
	}
	c.activeSessionLease = nil
}

// Installs the tool callbacks that need the live prompt UI once per process
func (c *CLI) installToolCallbacks() {
	if c.toolCallbacksInstalled {
		return
	}
	installInteractiveToolCallbacks(c)
	c.toolCallbacksInstalled = true
}

// Prints the startup banner for unacked security advisories on stderr, never blocking startup
func (c *CLI) showSecurityAdvisories() {
	if banner := SecurityAdvisoriesStartupBanner(c.ctx); banner != "" {
		fmt.Fprintln(stderrWriter(), banner)
	}
}

// Warns about toolsets switched off at startup by missing API keys or an unusable terminal backend
func (c *CLI) showToolAvailabilityWarnings() {
	_, unavailable := tools.Default.CheckToolAvailability(c.ctx, c.env)
	enabled := c.enabledToolsets
	if enabled == nil {
		enabled = []string{}
	}
	unavailable = tools.FilterToEnabledToolsets(unavailable, enabled, func(name string) ([]string, error) {
		return tools.Default.ResolveToolset(c.ctx, "", name), nil
	})
	backend, _ := tools.CurrentTerminalBackend(c.env)
	lines := tools.ToolAvailabilityWarningLines(unavailable, tools.TerminalBackendUnavailableReason(), backend)
	if len(lines) == 0 {
		return
	}
	c.consolePrint("")
	for _, line := range lines {
		c.consolePrint(line)
	}
}

// Starts worktree creation and the tool prewarm in the background, nil for a listing or when no worktree is wanted
func startWorktreeSetup(ctx context.Context, listTools, listToolsets, worktree, w bool) func() map[string]string {
	cfg := CLIConfig(ctx)
	if listTools || listToolsets || !(worktree || w || cliTruthy(cfg["worktree"])) {
		return nil
	}
	env := tools.ToolEnvFrom(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("tool prewarm failed: %v", r))
			}
		}()
		tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Quiet: true})
	}()
	syncBase := displayFlag(cfg, "worktree_sync", true)
	result := make(chan map[string]string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("worktree setup failed: %v", r))
				result <- nil
			}
		}()
		retainDB := filepath.Join(home.Dir(ctx), "state.db")
		result <- tools.SetupWorktree(ctx, tools.GitRepoRoot(), syncBase, "", &retainDB)
	}()
	return func() map[string]string {
		var info map[string]string
		select {
		case info = <-result:
		case <-time.After(120 * time.Second):
		}
		if len(info) == 0 {
			return nil
		}
		activeWorktreeMu.Lock()
		activeWorktree = info
		activeWorktreeMu.Unlock()
		_ = os.Setenv("TERMINAL_CWD", info["path"])
		if repo := tools.GitRepoRoot(); repo != "" {
			go func() {
				_ = tools.PruneStaleWorktrees(ctx, repo, 24)
				tools.MaintainPackHealth(tools.WithEnvHome(ctx, env), repo)
			}()
		}
		return info
	}
}

// Removes a clean worktree and its branch on exit, keeping recoverable work
func cleanupWorktree(ctx context.Context, info map[string]string) {
	activeWorktreeMu.Lock()
	if info == nil {
		info = activeWorktree
	}
	activeWorktreeMu.Unlock()
	if len(info) == 0 {
		return
	}
	wtPath, branch, repoRoot := info["path"], info["branch"], info["repo_root"]
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	activeWorktreeMu.Lock()
	activeWorktree = nil
	activeWorktreeMu.Unlock()
	infoAny := map[string]any{}
	for k, v := range info {
		infoAny[k] = v
	}
	if tools.ApplyExitChoice(ctx, infoAny, cprint) {
		return
	}
	if tools.WorktreeIsDirty(wtPath, repoRoot, 10*time.Second) {
		cprint(fmt.Sprintf("\n\033[33m%s\033[0m", i18n.T(ctx, "cli.worktree.uncommitted_keeping", "", map[string]any{"path": wtPath})))
		return
	}
	if tools.WorktreeHasUnpushedCommits(wtPath, 10*time.Second) {
		if tools.RepoIsShallow(repoRoot, 10*time.Second) {
			cprint(fmt.Sprintf("\n\033[33m%s\033[0m", i18n.T(ctx, "cli.worktree.shallow_clone_keeping", "", map[string]any{"path": wtPath})))
			fmt.Fprintf(console().Out, "  %s\n", i18n.T(ctx, "cli.worktree.next_session_deepens", "", nil))
		} else {
			cprint(fmt.Sprintf("\n\033[33m%s\033[0m", i18n.T(ctx, "cli.worktree.unpushed_keeping", "", map[string]any{"path": wtPath})))
			fmt.Fprintf(console().Out, "  %s\n", i18n.T(ctx, "cli.worktree.clean_up_manually", "", map[string]any{"path": wtPath}))
		}
		return
	}
	if blocked := tools.RemovalBlocked(wtPath); blocked != nil {
		cprint(fmt.Sprintf("\n\033[33m%s\033[0m", i18n.T(ctx, "cli.worktree.kept_reason", "", map[string]any{"path": wtPath, "reason": blocked.Reason})))
		return
	}
	tools.ReleaseLspClients(wtPath)
	tools.WorktreeGitQuiet([]string{"worktree", "unlock", wtPath}, repoRoot, 10*time.Second, "git worktree unlock failed (non-fatal)", nil)
	tools.WorktreeGitQuiet([]string{"worktree", "remove", wtPath, "--force"}, repoRoot, 15*time.Second, "Failed to remove worktree", nil)
	tools.WorktreeGitQuiet([]string{"branch", "-D", branch}, repoRoot, 10*time.Second, fmt.Sprintf("Failed to delete branch %s", branch), nil)
	cprint(fmt.Sprintf("\033[32m%s\033[0m", i18n.T(ctx, "cli.worktree.cleaned_up", "", map[string]any{"path": wtPath})))
}
