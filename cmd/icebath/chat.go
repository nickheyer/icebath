package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// --safe-mode or --ignore-user-config chat runs out of process under the gateway owner unless --tui forwards the flags
func bypassChatLaunch(a *chatArgs) bool {
	return (a.SafeMode || a.IgnoreUserConfig) && !a.TUI
}

// --cli wins, then --tui, then the TTY gate, then ICEBATH_TUI, then display.interface
func resolveUseTUI(a *chatArgs) bool {
	if a.CLI {
		return false
	}
	if a.TUI || a.TUINative {
		return true
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return false
	}
	if os.Getenv("ICEBATH_TUI") == "1" {
		return true
	}
	ctx := context.Background()
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(cfg.String("display.interface", "cli"))) == "tui"
}

// Flags the TUI session cannot forward refuse the launch before any session starts
func refuseUnforwardedTUIFlags(a *chatArgs) {
	var flags []string
	for _, pair := range [][2]string{{a.APIKey, "--api-key"}, {a.BaseURL, "--base-url"}, {a.Reasoning, "--reasoning"}} {
		if pair[0] != "" {
			flags = append(flags, pair[1])
		}
	}
	if len(flags) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: Unsupported --tui options: %s. No session was started.\n"+
		"  Use classic chat for this launch: icebath chat --cli %s ...\n"+
		"  Or set model.base_url / agent.reasoning_effort in config.yaml (/reasoning in the TUI).\n",
		strings.Join(flags, ", "), strings.Join(flags, " "))
	exit(2)
}

// Pins the active kanban board into the env so in process tools and shelled out calls agree
func pinKanbanBoardEnv() {
	if os.Getenv("ICEBATH_KANBAN_BOARD") != "" {
		return
	}
	if board := kanban.GetCurrentBoard(context.Background()); board != "" {
		os.Setenv("ICEBATH_KANBAN_BOARD", board)
	}
}

// Seeds the home skills directory with the bundled library, failures swallowed
func syncBundledSkillsQuietly() {
	_, _ = skills.SyncSkills(context.Background(), true)
}

// Git repo root when the cwd sits in a repo, else the cwd, empty when neither resolves
func resolveWorkspaceKey() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		if abs, err := filepath.Abs(strings.TrimSpace(string(out))); err == nil {
			return abs
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// Runs body against a read only session store of the home, nil when it cannot open, body errors swallowed
func withSessionDB(body func(db *gatewaycli.SurfaceLabelReader)) {
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		body(nil)
		return
	}
	defer db.Close()
	body(gatewaycli.NewSurfaceLabelReader(db))
}

// MRU session for the active interface, a TUI launch falling back to the CLI MRU
func latestSessionID(useTUI bool) string {
	source := "cli"
	if useTUI {
		source = "tui"
	}
	last := resolveLastSession(source)
	if last == "" && useTUI {
		last = resolveLastSession("cli")
	}
	return last
}

// Most recently used session of a source, scoped to the current workspace first
func resolveLastSession(source string) string {
	var sources []string
	if source == "cli" {
		for s := range agent.CLIFamilySources {
			sources = append(sources, s)
		}
		sortStrings(sources)
	} else {
		sources = []string{source}
	}
	var found string
	withSessionDB(func(db *gatewaycli.SurfaceLabelReader) {
		if db == nil {
			return
		}
		ctx := context.Background()
		if key := resolveWorkspaceKey(); key != "" {
			rows, err := db.SearchSessions(ctx, gatewaycli.SearchSessionsOptions{Sources: sources, Limit: 1, WorkspaceKey: key, SkipLaunchStubs: true})
			if err == nil && len(rows) > 0 {
				found = pyStr(rowValue(rows[0], "id"))
				return
			}
		}
		rows, err := db.SearchSessions(ctx, gatewaycli.SearchSessionsOptions{Sources: sources, Limit: 1, SkipLaunchStubs: true})
		if err == nil && len(rows) > 0 {
			found = pyStr(rowValue(rows[0], "id"))
		}
	})
	return found
}

// Session title or id to a session id projected onto the live compression tip, empty on a miss
func resolveSessionByNameOrID(nameOrID string) string {
	var resolved string
	withSessionDB(func(db *gatewaycli.SurfaceLabelReader) {
		if db == nil {
			return
		}
		ctx := context.Background()
		if session, err := db.GetSession(ctx, nameOrID); err == nil && session != nil {
			resolved = pyStr(rowValue(session, "id"))
		} else if byTitle, err := db.ResolveSessionByTitle(ctx, nameOrID); err == nil {
			resolved = byTitle
		}
		if resolved == "" && strings.HasPrefix(nameOrID, "local-") {
			if id, err := db.ResolveSessionID(ctx, nameOrID); err == nil {
				resolved = id
			}
		}
		if resolved != "" {
			if tip, err := db.GetCompressionTip(ctx, resolved); err == nil && tip != "" {
				resolved = tip
			}
		}
	})
	return resolved
}

// Creates a fresh titled session through the gateway writer, empty when it fails
func createTitledSession(title string) string {
	id, err := gatewaycli.CreateTitledSession(context.Background(), title)
	if err != nil {
		slogError(fmt.Sprintf("Failed to create titled session %s", config.PyRepr(title)), err)
		return ""
	}
	return id
}

// Resolves -c/--continue into the resume id, failing loudly on stderr for a missing name
func resolveContinueArg(a *chatArgs, useTUI bool) {
	if a.Continue == "" || a.Resume != "" {
		return
	}
	if a.Continue != flagPresent {
		if resolved := resolveSessionByNameOrID(a.Continue); resolved != "" {
			a.Resume = resolved
			return
		}
		if a.CreateIfMissing {
			if sid := createTitledSession(a.Continue); sid != "" {
				a.Resume = sid
				return
			}
			fmt.Fprintf(os.Stderr, "No session found matching '%s' and a new titled session could not be created.\n", a.Continue)
			exit(1)
		}
		fmt.Fprintf(os.Stderr, "No session found matching '%s'.\n", a.Continue)
		fmt.Fprintln(os.Stderr, "Use 'icebath sessions list' to see available sessions, or pass --create-if-missing to start a new session with that title.")
		exit(1)
	}
	if a.CreateIfMissing {
		fmt.Fprintln(os.Stderr, "--create-if-missing requires a session name: `-c <name> --create-if-missing`")
	}
	if crumb := app.ResolveBreadcrumbSession(context.Background()); crumb != "" {
		a.Resume = crumb
		return
	}
	if last := latestSessionID(useTUI); last != "" {
		a.Resume = last
		return
	}
	kind := "CLI"
	if useTUI {
		kind = "TUI"
	}
	fmt.Fprintf(os.Stderr, "No previous %s session to continue. Start a new one with `icebath`, or list sessions with `icebath sessions list`.\n", kind)
	exit(1)
}

// --in DIR changes directory first so workspace scoped lookups key off DIR and pin the session there
func applyInDir(a *chatArgs) {
	if a.In == "" {
		return
	}
	target := tools.MSYSToWindowsPath(a.In)
	if strings.HasPrefix(target, "~") {
		if userHome, err := os.UserHomeDir(); err == nil {
			target = userHome + target[1:]
		}
	}
	target, _ = filepath.Abs(target)
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		fmt.Printf("Error: --in directory not found: %s\n", a.In)
		exit(1)
	}
	if err := os.Chdir(target); err != nil {
		fmt.Printf("Error: cannot enter --in directory %s: %v\n", a.In, err)
		exit(1)
	}
	if strings.TrimSpace(os.Getenv("TERMINAL_CWD")) != "" {
		os.Setenv("TERMINAL_CWD", target)
	}
	a.NoRestoreCWD = true
}

// --resume @claude or @codex imports a foreign session and resumes it
func importForeignResume(a *chatArgs) {
	lowered := strings.ToLower(strings.TrimSpace(a.Resume))
	if lowered != "@claude" && lowered != "@codex" {
		return
	}
	ctx := context.Background()
	picked := app.PickForeignSession(ctx, strings.TrimPrefix(lowered, "@"))
	if picked == nil {
		exit(1)
	}
	imported, err := app.ImportForeignSession(ctx, picked.Source, picked.Path)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		exit(1)
	}
	fmt.Printf("✓ Imported as %s — resuming it now.\n", imported)
	fmt.Printf("  (later: icebath --resume %s)\n", imported)
	a.Resume = imported
}

// The cwd the owner runs the session's tools in from its local policy, empty without one
func frozenCwd(db *gatewaycli.SurfaceLabelReader, sessionID string) string {
	receipt, err := db.LocalReceipt(context.Background(), sessionID)
	if err != nil || receipt == nil {
		return ""
	}
	policy, ok := rowValue(receipt, "policy").(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(pyStrOrEmpty(policy["cwd"]))
}

// A resumed session whose frozen cwd was a removed worktree gets the tree back before attaching
func recreateResumedTUIWorktree(a *chatArgs) {
	if a.Resume == "" || a.Worktree {
		return
	}
	var cwd string
	withSessionDB(func(db *gatewaycli.SurfaceLabelReader) {
		if db == nil {
			return
		}
		cwd = frozenCwd(db, a.Resume)
		if cwd == "" {
			if session, err := db.GetSession(context.Background(), a.Resume); err == nil && session != nil {
				cwd = strings.TrimSpace(pyStr(rowValue(session, "cwd")))
			}
		}
	})
	if cwd == "" {
		return
	}
	if st, err := os.Stat(cwd); err == nil && st.IsDir() {
		return
	}
	if tree := tools.RecreateTUIWorktree(context.Background(), cwd, a.Resume); tree != "" {
		fmt.Printf("↪ recreated the session's worktree: %s\n", tree)
	}
}

// Normalizes --in, --resume and --continue before agent init and restores a resumed session's cwd
func resolveChatSessionArgs(a *chatArgs, useTUI bool) {
	applyInDir(a)
	if strings.ToLower(strings.TrimSpace(a.Resume)) == "latest" {
		if last := latestSessionID(useTUI); last != "" {
			a.Resume = last
		} else {
			kind := "CLI"
			if useTUI {
				kind = "TUI"
			}
			fmt.Printf("No previous %s session found to resume.\n", kind)
			fmt.Println("Use 'icebath sessions list' to see available sessions.")
			exit(1)
		}
	}
	resolveContinueArg(a, useTUI)
	importForeignResume(a)
	if a.Resume != "" {
		if resolved := resolveSessionByNameOrID(a.Resume); resolved != "" {
			a.Resume = resolved
		}
	}
	recreateResumedTUIWorktree(a)
	if a.Resume == "" || a.NoRestoreCWD || a.Worktree {
		return
	}
	withSessionDB(func(db *gatewaycli.SurfaceLabelReader) {
		if db == nil {
			return
		}
		session, err := db.GetSession(context.Background(), a.Resume)
		if err != nil || session == nil {
			return
		}
		saved := strings.TrimSpace(pyStr(rowValue(session, "cwd")))
		if saved == "" {
			return
		}
		cwd, _ := os.Getwd()
		if st, err := os.Stat(saved); err != nil || !st.IsDir() {
			fmt.Printf("⚠ session's recorded dir is gone (%s); staying in %s\n", saved, cwd)
			return
		}
		if realpath(saved) != realpath(cwd) {
			if err := os.Chdir(saved); err == nil {
				fmt.Printf("↪ restored workspace dir: %s\n", saved)
			}
		}
	})
}

// One shot xAI retirement warning on stderr, never failing startup
func warnRetiredXAIModels() {
	ctx := context.Background()
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return
	}
	refs := provider.FindRetiredXAIRefs(map[string]any(cfg))
	if len(refs) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\033[33m⚠ xAI retires %d model(s) in your config on %s:\033[0m\n", len(refs), provider.XAIRetirementDate)
	for _, ref := range refs {
		fmt.Fprintf(os.Stderr, "  \033[33m⚠\033[0m %s\n", provider.FormatRetirementIssue(ref))
	}
	fmt.Fprintf(os.Stderr, "  \033[2mMigration guide: %s\033[0m\n", provider.XAIMigrationGuideURL)
	fmt.Fprint(os.Stderr, "  \033[2mRun 'icebath doctor' for details.\033[0m\n\n")
}

// True when the home .env assigns a non empty value to any provider key
func dotenvHasProviderKey(envFile string, providerEnvVars map[string]bool) bool {
	raw, err := os.ReadFile(envFile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, _ := strings.Cut(line, "=")
		if providerEnvVars[strings.TrimSpace(key)] && strings.Trim(strings.TrimSpace(val), "'\"") != "" {
			return true
		}
	}
	return false
}

// True when auth.json's active provider is logged in, api key providers ignored under strict scope
func authStoreLoggedIn(authFile string, rows []*provider.Profile, strictProfileScope bool, env provider.PoolEnv) bool {
	if _, err := os.Stat(authFile); err != nil {
		return false
	}
	auth, err := env.Store.LoadFile(authFile)
	if err != nil {
		return false
	}
	activeRaw := auth["active_provider"]
	if !truthy(activeRaw) {
		return false
	}
	active := strings.ToLower(strings.TrimSpace(pyStr(activeRaw)))
	var activeConfig *provider.Profile
	for _, row := range rows {
		if row.Name == active {
			activeConfig = row
			break
		}
	}
	if strictProfileScope && activeConfig != nil && activeConfig.AuthType == provider.AuthTypeAPIKey {
		return false
	}
	return truthy(provider.GetAuthStatus(env, pyStr(activeRaw))["logged_in"])
}

// Whether at least one inference provider is usable, never creating one
func hasAnyProviderConfigured(strictProfileScope bool) bool {
	ctx := context.Background()
	h := home.Dir(ctx)
	cfg, err := config.Load(h, nil)
	if err != nil {
		cfg = config.Tree{}
	}
	modelCfg := cfg["model"]
	modelName := ""
	switch m := modelCfg.(type) {
	case string:
		modelName = m
	case map[string]any:
		switch def := m["default"].(type) {
		case map[string]any:
			modelName, _ = config.SplitModelDefault(def)
		default:
			modelName = pyStr(def)
			if def == nil {
				modelName = ""
			}
		}
	}
	modelName = strings.TrimSpace(modelName)
	hasIcebathConfig := modelName != "" && modelName != pyStr(config.Defaults()["model"])
	providerEnvVars := map[string]bool{"OPENROUTER_API_KEY": true, "OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_TOKEN": true, "OPENAI_BASE_URL": true}
	rows := provider.CanonicalRegistryRows(ctx)
	for _, row := range rows {
		if row.AuthType == provider.AuthTypeAPIKey {
			for _, v := range row.KeyEnv {
				providerEnvVars[v] = true
			}
		}
	}
	read := os.Getenv
	if strictProfileScope {
		scope := provider.CurrentSecretScope(ctx)
		read = func(name string) string { return scope[name] }
	}
	for v := range providerEnvVars {
		if read(v) != "" {
			return true
		}
	}
	if dotenvHasProviderKey(home.EnvPath(h), providerEnvVars) {
		return true
	}
	env := provider.HomePoolEnv(h)
	if authStoreLoggedIn(home.AuthPath(h), rows, strictProfileScope, env) {
		return true
	}
	if m, ok := modelCfg.(map[string]any); ok {
		for _, k := range []string{"provider", "base_url", "api_key"} {
			if strings.TrimSpace(pyStr(m[k])) != "" && m[k] != nil {
				return true
			}
		}
	}
	if !strictProfileScope {
		for _, row := range rows {
			if row.AuthType != provider.AuthTypeAPIKey {
				continue
			}
			status := provider.GetAuthStatus(env, row.Name)
			if truthy(status["logged_in"]) && pyStr(status["key_source"]) != "keyless" {
				return true
			}
		}
	}
	if hasIcebathConfig && !strictProfileScope {
		if creds := provider.ReadClaudeCodeCredentials(env); creds != nil && (provider.IsClaudeCodeTokenValid(creds) || truthy(creds["refreshToken"])) {
			return true
		}
	}
	return false
}

// Guards startup -m and --provider overrides before the first API call
func confirmStartupExpensiveModelOverride(a *chatArgs) {
	explicitModel, explicitProvider := strings.TrimSpace(a.Model), strings.TrimSpace(a.Provider)
	if explicitModel == "" && explicitProvider == "" {
		return
	}
	ctx := context.Background()
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		slogWarn(fmt.Sprintf("startup model cost guard could not load config: %v", err))
		cfg = config.Tree{}
	}
	modelCfg := cfg.Map("model")
	securityCfg := cfg.Map("security")
	model := explicitModel
	if model == "" {
		model = strings.TrimSpace(pyStrOrEmpty(modelCfg["default"]))
	}
	if model == "" {
		return
	}
	providerName := explicitProvider
	if providerName == "" {
		providerName = strings.TrimSpace(pyStrOrEmpty(modelCfg["provider"]))
	}
	warnings := provider.SelectionWarnings(ctx, provider.SelectionRequest{Model: model, Provider: providerName,
		BaseURL: pyStrOrEmpty(modelCfg["base_url"]), APIKey: pyStrOrEmpty(modelCfg["api_key"])})
	if len(warnings) == 0 {
		return
	}
	interactive := isatty.IsTerminal(os.Stdin.Fd())
	if !interactive && securityCfg["allow_data_training_tiers_noninteractive"] == true {
		var acknowledged, rest []provider.SelectionWarning
		for _, w := range warnings {
			if w.Kind == "data_policy" {
				acknowledged = append(acknowledged, w)
			} else {
				rest = append(rest, w)
			}
		}
		if len(acknowledged) > 0 {
			fmt.Fprintln(os.Stderr, provider.CombinedSelectionMessage(acknowledged))
			fmt.Fprintln(os.Stderr, "Proceeding in non-interactive mode because security.allow_data_training_tiers_noninteractive is true.")
			warnings = rest
			if len(warnings) == 0 {
				return
			}
		}
	}
	message := provider.CombinedSelectionMessage(warnings)
	if !interactive {
		fmt.Fprintln(os.Stderr, message)
		for _, w := range warnings {
			if w.Kind == "data_policy" {
				fmt.Fprintln(os.Stderr, "To acknowledge data-training tiers for unattended runs, set security.allow_data_training_tiers_noninteractive to true in config.yaml.")
				break
			}
		}
		fmt.Fprintln(os.Stderr, "Refusing this startup model override in non-interactive mode. Run interactively and confirm if you intend to use it.")
		exit(1)
	}
	fmt.Fprintln(os.Stderr, message)
	fmt.Print("Use this model for this invocation? [y/N] ")
	reply := strings.ToLower(strings.TrimSpace(readLine()))
	if reply != "y" && reply != "yes" {
		fmt.Fprintln(os.Stderr, "Model override cancelled.")
		exit(1)
	}
}

// One line of stdin, empty at EOF
func readLine() string {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// No provider configured, offers icebath setup on a TTY or exits 1 with guidance
func firstRunSetupGuard(a *chatArgs) {
	fmt.Println()
	fmt.Println("It looks like Icebath isn't configured yet -- no API keys or providers found.")
	fmt.Println()
	fmt.Println("  Run:  icebath setup")
	fmt.Println()
	if !app.IsInteractiveStdin() {
		app.PrintNoninteractiveSetupGuidance("No interactive TTY detected for the first-run setup prompt.")
		exit(1)
	}
	fmt.Print("Run setup now? [Y/n] ")
	reply := strings.ToLower(strings.TrimSpace(readLine()))
	if reply == "" || reply == "y" || reply == "yes" {
		exit(cmdSetup(a))
	}
	fmt.Println()
	fmt.Println("You can run 'icebath setup' at any time to configure.")
	exit(1)
}

// Termux bundled skills sync fingerprint, the checkout revision or the skills tree stat
func termuxBundledSkillsFingerprint() string {
	root := pm.RepoRoot()
	if fp, ok := pm.ReadGitRevisionFingerprint(root); ok {
		return fp
	}
	info, _ := updaterecord.GetVersionInfo()
	identity := info.DerivedVersion
	if info.Commit != nil && *info.Commit != "" {
		identity = *info.Commit
	}
	st, err := os.Stat(filepath.Join(root, "skills"))
	if err != nil {
		return fmt.Sprintf("skills:%s:missing", identity)
	}
	return fmt.Sprintf("skills:%s:%d:%d", identity, st.ModTime().UnixNano(), st.Size())
}

func termuxBundledSkillsStampPath() string {
	return filepath.Join(home.Dir(context.Background()), "skills", ".termux_bundled_sync_stamp")
}

func termuxBundledSkillsSyncNeeded() bool {
	if !isTermuxStartupEnvironment() || os.Getenv("ICEBATH_TERMUX_FORCE_SKILLS_SYNC") == "1" {
		return true
	}
	raw, err := os.ReadFile(termuxBundledSkillsStampPath())
	if err != nil {
		return true
	}
	return strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff")) != termuxBundledSkillsFingerprint()
}

func markTermuxBundledSkillsSynced() {
	if !isTermuxStartupEnvironment() {
		return
	}
	stamp := termuxBundledSkillsStampPath()
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(stamp, []byte(termuxBundledSkillsFingerprint()+"\n"), 0o644)
}

// Syncs bundled skills, skipping unchanged Termux checkouts cheaply
func syncBundledSkillsForStartup() bool {
	if isTermuxStartupEnvironment() && !termuxBundledSkillsSyncNeeded() {
		return false
	}
	_, _ = skills.SyncSkills(context.Background(), true)
	markTermuxBundledSkillsSynced()
	return true
}

func termuxShouldPrefetchUpdateCheck() bool {
	return !isTermuxStartupEnvironment() || os.Getenv("ICEBATH_TERMUX_PREFETCH_UPDATES") == "1"
}

// Kicks off the update check and banner prefetch and the bundled skills sync
func startChatBackgroundPrefetch() {
	if termuxShouldPrefetchUpdateCheck() {
		app.PrefetchUpdateCheck()
		app.PrefetchBannerData()
	}
	unseeded := func() bool {
		dir := filepath.Join(home.Dir(context.Background()), "skills")
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return true
		}
		found := false
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == "SKILL.md" {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		return !found
	}
	sync := func() {
		defer func() { recover() }()
		syncBundledSkillsForStartup()
	}
	if unseeded() {
		sync()
		app.ResetAvailableSkillsCache()
		return
	}
	go sync()
}

// Runs the interactive chat CLI or the TUI, hermes_cli/main.py cmd_chat
func cmdChat(a *chatArgs) int {
	streamJSON, code := app.StreamJSONRequested(app.StreamJSONArgs{OutputFormat: a.OutputFormat, Query: a.Query, QueryFile: a.QueryFile, TUI: a.TUI, Quiet: &a.Quiet})
	if code != 0 {
		return code
	}
	if streamJSON || bypassChatLaunch(a) {
		return launchGatewayChat(a)
	}
	applySafeMode(a)
	applyUserConfigBypass(a)
	guardNoninteractiveUserConfig(a)
	useTUI := resolveUseTUI(a)
	if useTUI {
		refuseUnforwardedTUIFlags(a)
	}
	resolveChatSessionArgs(a, useTUI)
	warnRetiredXAIModels()
	providerConfigured := hasAnyProviderConfigured(false)
	if !useTUI && !providerConfigured {
		firstRunSetupGuard(a)
		return 0
	}
	startChatBackgroundPrefetch()
	if a.Yolo {
		os.Setenv("ICEBATH_YOLO_MODE", "1")
	}
	if a.IgnoreRules {
		os.Setenv("ICEBATH_IGNORE_RULES", "1")
	}
	if a.Source != "" {
		os.Setenv("ICEBATH_SESSION_SOURCE", a.Source)
		os.Setenv("ICEBATH_SESSION_SOURCE_EXPLICIT", "1")
	}
	pinKanbanBoardEnv()
	ctx := context.Background()
	if providerConfigured {
		observability.OfferConsentBeforeChat(ctx, observability.ConsentChatArgs{Query: a.Query, QueryFile: a.QueryFile, Oneshot: a.Oneshot,
			OneshotExit: a.OneshotExit, SafeMode: a.SafeMode, IgnoreUserConfig: a.IgnoreUserConfig})
	}
	confirmStartupExpensiveModelOverride(a)
	if useTUI {
		return app.LaunchTUI(ctx, app.TUILaunch{Resume: a.Resume, Dev: a.TUIDev, Native: a.TUINative, Model: a.Model, AcceptHooks: a.AcceptHooks,
			Provider: a.Provider, Toolsets: a.Toolsets, Skills: a.Skills, Verbose: a.Verbose, Quiet: a.Quiet, Query: a.Query, Image: a.Image,
			Worktree: a.Worktree, Checkpoints: a.Checkpoints, PassSessionID: a.PassSessionID, MaxTurns: a.MaxTurns})
	}
	readQueryFile(a)
	observability.BeginProcess(ctx, "cli")
	return app.Main(ctx, app.MainOptions{Model: a.Model, Reasoning: a.Reasoning, Toolsets: a.Toolsets, Query: a.Query, Oneshot: a.OneshotExit,
		RunBudget: a.RunBudget, OutputFormat: a.OutputFormat, IgnoreRules: a.IgnoreRules || a.SafeMode, IgnoreUserConfig: a.IgnoreUserConfig || a.SafeMode,
		Compact: a.Compact, Provider: a.Provider, Skills: a.Skills, Verbose: a.Verbose, Quiet: a.Quiet, Image: a.Image, Resume: a.Resume,
		Worktree: a.Worktree, Checkpoints: a.Checkpoints, PassSessionID: a.PassSessionID, MaxTurns: a.MaxTurns, APIKey: a.APIKey, BaseURL: a.BaseURL})
}

// --query-file reads the single query from a file or stdin with a dash
func readQueryFile(a *chatArgs) {
	if a.QueryFile == "" {
		return
	}
	if a.Query != "" {
		fmt.Fprintln(os.Stderr, "Error: -q/--query and --query-file are mutually exclusive")
		exit(2)
	}
	var raw []byte
	var err error
	if a.QueryFile == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(a.QueryFile)
		raw = []byte(strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\uFFFD"), "\ufeff"))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot read --query-file %s: %v\n", a.QueryFile, err)
		exit(2)
	}
	a.Query = string(raw)
	if strings.TrimSpace(a.Query) == "" {
		fmt.Fprintf(os.Stderr, "Error: --query-file %s is empty\n", a.QueryFile)
		exit(2)
	}
}

// Frontend startup check shared by classic chat and top level one shot, false when setup was offered
func ensureLaunchProvider(a *chatArgs) bool {
	if gateway.BypassLaunch(a.SafeMode, a.IgnoreUserConfig, a.TUI) {
		return true
	}
	if a.Resume != "" || strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL")) != "" || a.BaseURL != "" || a.APIKey != "" {
		return true
	}
	provider.RunFreeTierBootstrap(provider.HomePoolEnv(home.Dir(context.Background())), false)
	if hasAnyProviderConfigured(false) {
		return true
	}
	firstRunSetupGuard(a)
	return false
}

// The transport launcher of a gateway run chat, hermes_cli/gateway_chat_startup.py launch_gateway_chat
func launchGatewayChat(a *chatArgs) int {
	if !gateway.BypassLaunch(a.SafeMode, a.IgnoreUserConfig, a.TUI) {
		confirmStartupExpensiveModelOverride(a)
	}
	return gateway.LaunchChatFromArgs(context.Background(), gateway.ChatLaunchArgs{Query: a.Query, QueryFile: a.QueryFile, Oneshot: a.Oneshot,
		OneshotExit: a.OneshotExit, Model: a.Model, Provider: a.Provider, Toolsets: a.Toolsets, Skills: a.Skills, Reasoning: a.Reasoning,
		Resume: a.Resume, UsageFile: a.UsageFile, SafeMode: a.SafeMode, IgnoreUserConfig: a.IgnoreUserConfig, IgnoreRules: a.IgnoreRules,
		TUI: a.TUI, Yolo: a.Yolo, AcceptHooks: a.AcceptHooks, OutputFormat: a.OutputFormat, Quiet: a.Quiet, Verbose: a.Verbose, Image: a.Image,
		APIKey: a.APIKey, BaseURL: a.BaseURL, EnsureProvider: ensureLaunchProvider(a)})
}
