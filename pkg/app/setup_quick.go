package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

var setupQuickLog = logging.GetLogger("hermes_cli.setup")

// Seams over the Nous flow, the bundled skill calls and the macOS probe, the names the setup_quick tests patch
var (
	setupSetBundledSkillsOptOut = func(ctx context.Context, optOut bool) map[string]any {
		return skills.NewScope(home.Dir(ctx), nil).SetBundledSkillsOptOut(optOut)
	}
	setupSyncSkills     = skills.SyncSkills
	setupWalkthroughFn  = blankSlateWalkthrough
	setupSysPlatform    = runtime.GOOS
	setupUserHomeDir    = pypath.PathHome
	setupReadDirNames   = func(path string) error { _, err := os.ReadDir(path); return err }
	setupRunToolsPicker = func(ctx context.Context, cfg config.Tree) error { return provider.SetupTools(ctx, cfg, false) }
)

// Messaging env name fragments, their platform labels and emoji, the first match winning
var setupMessagingPlatforms = [][3]string{{"TELEGRAM", "Telegram", "📱"}, {"DISCORD", "Discord", "💬"}, {"SLACK", "Slack", "💼"}}

// Shared Blank Slate epilogue, the success line, the enable later hints and the summary
func blankSlateDone(ctx context.Context, cfg config.Tree, icebathHome, toolsLine string, extra []string, intro string) error {
	setupPrintln("")
	provider.SetupPrintSuccess("Blank Slate setup complete — minimal agent ready.")
	var lines []any
	if intro != "" {
		lines = append(lines, intro)
	}
	lines = append(lines, toolsLine, "  Seed skills:         icebath skills opt-in --sync", "  Add MCP servers:     icebath mcp add")
	for _, line := range extra {
		lines = append(lines, line)
	}
	lines = append(lines, "  Tune agent settings: icebath setup agent", nil)
	provider.SetupInfo(lines...)
	return setupPrintSetupSummaryFn(ctx, cfg, icebathHome)
}

// Provider and model an agent import left in the config when credentials back it, empty otherwise
func importedModelLabel(ctx context.Context, cfg config.Tree) (string, error) {
	var name any = cfg["model"]
	if model, ok := cfg["model"].(map[string]any); ok {
		name = model["default"]
	}
	text, isText := name.(string)
	if !isText || pystr.Strip(text) == "" {
		return "", nil
	}
	ok, err := provider.ModelSectionHasCredentials(ctx, cfg)
	if err != nil || !ok {
		return "", err
	}
	env := provider.HomePoolEnv(home.Dir(ctx))
	env.Ctx = ctx
	resolved, err := provider.ResolveAuthProvider(env, "")
	var authErr *provider.AuthError
	if errors.As(err, &authErr) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return provider.ProviderLabel(resolved) + " · " + pystr.Strip(text), nil
}

// Asks whether to keep the model an import brought or switch to Nous Portal, true keeping it
func keepImportedModel(ctx context.Context, cfg config.Tree, importedFrom string) (bool, error) {
	label := ""
	if importedFrom != "" {
		var err error
		if label, err = importedModelLabel(ctx, cfg); err != nil {
			return false, err
		}
	}
	if label == "" {
		return false, nil
	}
	provider.PrintSetupHeader("Model & Provider", true)
	provider.SetupInfo("Settings were imported from " + importedFrom + ".")
	return provider.PromptChoice("Keep your imported model ("+label+") or switch to Nous Portal?", []string{
		"Keep " + label,
		"Switch to Nous Portal — free OAuth login, 300+ models plus the Tool Gateway",
	}, 0, "  Settings were imported from "+importedFrom+".") == 0, nil
}

// Quick Setup's Nous Portal step, the OAuth login and model pick, then the config resynced from disk
func quickSetupNousPortal(ctx context.Context, cfg config.Tree) error {
	provider.PrintSetupHeader("Nous Portal", true)
	provider.SetupInfo("One subscription, 300+ models, plus the Tool Gateway:",
		"  web search, image generation, TTS, browser automation.",
		"Sign up: https://portal.nousresearch.com/manage-subscription", nil)
	onError := func(err error) {
		lines := provider.ProviderSetupFailureLines(err, "icebath model")
		provider.SetupPrintWarning(lines[0])
		for _, line := range lines[1:] {
			provider.SetupPrintInfo(line)
		}
	}
	if _, err := provider.RunNousFlow(ctx, cfg, "quick setup", false, []any{nil, "Nous Portal setup cancelled."}, onError); err != nil {
		return err
	}
	return provider.ReloadSetupConfig(ctx, cfg, false)
}

// First time Quick Setup through Nous Portal or an imported model, then terminal and messaging with defaults elsewhere
func runFirstTimeQuickSetup(ctx context.Context, cfg config.Tree, icebathHome, importedFrom string) error {
	h := home.Dir(ctx)
	keep, err := keepImportedModel(ctx, cfg, importedFrom)
	if err != nil {
		return err
	}
	if !keep {
		if err := quickSetupNousPortal(ctx, cfg); err != nil {
			return err
		}
	}
	if err := setupTerminalBackendFn(ctx, cfg); err != nil {
		return err
	}
	if err := provider.ApplyDefaultAgentSettings(ctx, cfg); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	setupPrintln("")
	gatewayChoice := provider.PromptChoice("Connect a messaging platform? (Telegram, Discord, etc.)", []string{
		"Set up messaging now (recommended)", "Skip — set up later with 'icebath setup gateway'",
	}, 0, "")
	if gatewayChoice == 0 {
		if err := setupGatewayFn(ctx, cfg); err != nil {
			return err
		}
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return err
		}
	} else if _, err := setupEnsureGatewayService(ctx, provider.SetupOutput(), gateway.ServiceSetupOptions{Context: "setup", Interactive: true, Config: cfg}); err != nil {
		return err
	}
	setupPrintln("")
	provider.SetupPrintSuccess("Setup complete! You're ready to go.")
	provider.SetupInfo(nil, "  Configure all settings:    icebath setup")
	if gatewayChoice != 0 {
		provider.SetupPrintInfo("  Connect Telegram/Discord:  icebath setup gateway")
	}
	if err := printMacosFDATip(); err != nil {
		return err
	}
	setupPrintln("")
	return setupPrintSetupSummaryFn(ctx, cfg, icebathHome)
}

// One time macOS tip that a single Full Disk Access grant silences every folder prompt, silent when granted or unknown
func printMacosFDATip() error {
	if setupSysPlatform != "darwin" {
		return nil
	}
	userHome, err := setupUserHomeDir()
	if err != nil {
		return err
	}
	err = setupReadDirNames(filepath.Join(userHome, "Library", "Application Support", "com.apple.TCC"))
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return nil
	}
	provider.SetupInfo(nil, "  macOS tip: silence ALL folder permission prompts with one switch —",
		"  System Settings → Privacy & Security → Full Disk Access → enable",
		"  your terminal (and Icebath.app if you use Desktop), or run:",
		"    open \"x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles\"",
		"  The grant is permanent — it survives every Icebath update.")
	return nil
}

// Records the bundled skills opt out marker and syncs, essential skills always seeded, reporting the outcome
func setBundledSkillsOptOut(ctx context.Context, optOut bool, logLabel string, onSuccess func(map[string]any), onError func(error)) {
	setupSetBundledSkillsOptOut(ctx, optOut)
	result, err := setupSyncSkills(ctx, true)
	if err != nil {
		setupQuickLog.Debug(fmt.Sprintf("blank-slate %s error: %v", logLabel, err))
		if onError != nil {
			onError(err)
		}
		return
	}
	if onSuccess != nil {
		onSuccess(result)
	}
}

// Blank Slate setup, the essentials forced on and everything else off, then finish or walk through opting in
func runBlankSlateSetup(ctx context.Context, cfg config.Tree, icebathHome string) error {
	h := home.Dir(ctx)
	provider.PrintSetupHeader("Blank Slate Setup", true)
	provider.SetupInfo("Everything starts OFF. First we force-enable only what's required",
		"to run an agent, then you choose whether to stop there or walk",
		"through enabling more — opting in to exactly what you want.", "",
		"Forced on: Provider & Model, File Operations, Terminal, Vision, Skills.",
		"Everything else (web, browser, code exec, memory,",
		"delegation, cron, plugins, MCP, …) starts disabled. The",
		"essential `icebath-agent` skill is always kept so the agent",
		"can help you drive and configure Icebath itself.", nil)
	provider.PrintSetupHeader("Step 1 — Provider & Model (required)", false)
	if err := setupModelProviderFn(ctx, cfg); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	provider.PrintSetupHeader("Step 2 — Terminal Backend", false)
	if err := setupTerminalBackendFn(ctx, cfg); err != nil {
		return err
	}
	if err := tools.BlankSlateMinimalToolsets(ctx, cfg); err != nil {
		return err
	}
	if err := tools.BlankSlateMinimizeConfig(cfg); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	setupPrintln("")
	provider.SetupPrintSuccess("Minimal baseline applied:")
	provider.SetupPrintInfo("  Toolsets: file, terminal, vision, skills (everything else off)")
	provider.SetupPrintInfo("  Compression, memory, checkpoints, smart routing: off")
	provider.PrintSetupHeader("How far do you want to go?", true)
	path := provider.PromptChoice("Your minimal agent is ready. What next?", []string{
		"Start with everything disabled — finish now (most minimal)",
		"Walk through all configurations — opt in to tools, skills, plugins, MCP",
	}, 0, "")
	if path != 0 {
		return setupWalkthroughFn(ctx, cfg, icebathHome)
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	setBundledSkillsOptOut(ctx, true, "skill opt-out", nil, nil)
	return blankSlateDone(ctx, cfg, icebathHome, "  Enable tools:        icebath tools",
		[]string{"  Enable plugins:      icebath plugins"}, "Enable anything later, on demand:")
}

// Blank Slate opt in walkthrough over skills, tools, plugins, MCP servers and messaging
func blankSlateWalkthrough(ctx context.Context, cfg config.Tree, icebathHome string) error {
	h := home.Dir(ctx)
	provider.PrintSetupHeader("Bundled Skills", true)
	provider.SetupPrintInfo("Blank Slate ships with NO bundled skills by default.")
	seedSkills := provider.PromptYesNo("Seed the full bundled skill catalog? (No = start with zero skills)", false, "")
	seeded := func(result map[string]any) {
		copied, _ := result["copied"].([]string)
		provider.SetupPrintSuccess(fmt.Sprintf("Seeded %d bundled skills.", len(copied)))
	}
	optedOut := func(map[string]any) {
		provider.SetupInfo("No skills seeded (except the essential `icebath-agent`",
			"skill). A .no-bundled-skills marker keeps future",
			"`icebath update` runs from re-injecting them. Opt back in any",
			"time with `icebath skills opt-in --sync`.")
	}
	onSuccess := optedOut
	if seedSkills {
		onSuccess = seeded
	}
	setBundledSkillsOptOut(ctx, !seedSkills, "skill handling", onSuccess, func(err error) {
		provider.SetupPrintWarning(fmt.Sprintf("Skill setup step encountered an error: %v", err))
	})
	provider.PrintSetupHeader("Tools", true)
	provider.SetupInfo("Pick exactly which additional toolsets to turn on.",
		"(file and terminal are already on; leave the rest off if you want", " the most minimal agent.)")
	if provider.PromptYesNo("Open the tool selector to enable more tools?", false, "") {
		err := setupRunToolsPicker(ctx, cfg)
		if err == nil {
			err = provider.ReloadSetupConfig(ctx, cfg, false)
		}
		if err != nil {
			setupQuickLog.Debug(fmt.Sprintf("blank-slate tools_command error: %v", err))
			provider.SetupPrintWarning(fmt.Sprintf("Tool selector encountered an error: %v", err))
		}
	} else {
		provider.SetupPrintInfo("Keeping the minimal toolset. Add tools later with `icebath tools`.")
	}
	for _, step := range [][4]string{
		{"Plugins", "Review and enable built-in plugins now?",
			"Manage plugins with `icebath plugins list` / `icebath plugins install`.",
			"No plugins enabled. Add later with `icebath plugins`."},
		{"MCP Servers", "Add an MCP server now?",
			"Add servers with `icebath mcp add <name> --url ... | --command ...`.",
			"No MCP servers configured. Add later with `icebath mcp add`."},
	} {
		provider.PrintSetupHeader(step[0], true)
		if provider.PromptYesNo(step[1], false, "") {
			provider.SetupPrintInfo(step[2])
		} else {
			provider.SetupPrintInfo(step[3])
		}
	}
	setupPrintln("")
	if provider.PromptYesNo("Connect a messaging platform (Telegram, Discord, …)?", false, "") {
		if err := setupGatewayFn(ctx, cfg); err != nil {
			return err
		}
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	return blankSlateDone(ctx, cfg, icebathHome, "  Enable more tools:   icebath tools", nil, "")
}

// Description of a missing env var, its name when it has none
func missingEnvDescription(v config.MissingEnvVar) string {
	if v.Description != "" {
		return v.Description
	}
	return v.Name
}

// Quick setup of an existing install, prompting only for the items still missing
func runQuickSetup(ctx context.Context, cfg config.Tree, icebathHome string) error {
	h := home.Dir(ctx)
	provider.PrintSetupHeader("Quick Setup — Missing Items Only", true)
	missingEnv, err := config.GetMissingEnvVars(ctx, h, false)
	if err != nil {
		return err
	}
	var missingRequired, missingOptional []config.MissingEnvVar
	for _, v := range missingEnv {
		if v.IsRequired {
			missingRequired = append(missingRequired, v)
		} else {
			missingOptional = append(missingOptional, v)
		}
	}
	missingConfig, err := config.GetMissingConfigFields(ctx, h, nil)
	if err != nil {
		return err
	}
	currentVer, latestVer, err := config.CheckConfigVersion(ctx, h, false)
	if err != nil {
		return err
	}
	if len(missingRequired) == 0 && len(missingOptional) == 0 && len(missingConfig) == 0 && currentVer >= latestVer {
		provider.SetupPrintSuccess("Everything is configured! Nothing to do.")
		provider.SetupInfo(nil, "Run 'icebath setup' and choose 'Full Setup' to reconfigure,",
			"or pick a specific section from the menu.")
		return nil
	}
	if len(missingRequired) > 0 {
		provider.SetupInfo(nil, fmt.Sprintf("%d required setting(s) missing:", len(missingRequired)))
		for _, v := range missingRequired {
			setupPrintln("     • " + v.Name)
		}
		setupPrintln("")
		for _, v := range missingRequired {
			setupPrintln("")
			setupPrintln(setupColor("  "+v.Name, cliui.Cyan))
			provider.SetupPrintInfo("  " + v.Description)
			if v.URL != "" {
				provider.SetupPrintInfo("  Get key at: " + v.URL)
			}
			if err := promptAndSaveEnvVar(ctx, v, "  Saved "+v.Name, "  Skipped "+v.Name); err != nil {
				return err
			}
		}
	}
	var missingTools, missingMessaging []config.MissingEnvVar
	for _, v := range missingOptional {
		switch {
		case v.Category == "tool":
			missingTools = append(missingTools, v)
		case v.Category == "messaging" && !v.Advanced:
			missingMessaging = append(missingMessaging, v)
		}
	}
	if len(missingTools) > 0 {
		provider.PrintSetupHeader("Tool API Keys", true)
		labels := make([]string, len(missingTools))
		for i, v := range missingTools {
			labels[i] = missingEnvDescription(v)
			if len(v.Tools) > 0 {
				labels[i] += " → " + strings.Join(v.Tools[:min(2, len(v.Tools))], ", ")
			}
		}
		for _, idx := range provider.PromptChecklist("Which tools would you like to configure?", labels, nil) {
			if err := promptToolAPIKey(ctx, missingTools[idx]); err != nil {
				return err
			}
		}
	}
	if len(missingMessaging) > 0 {
		provider.PrintSetupHeader("Messaging Platforms", true)
		provider.SetupInfo("Connect Icebath to messaging apps to chat from anywhere.",
			"You can configure these later with 'icebath setup gateway'.")
		grouped := map[string][]config.MissingEnvVar{}
		emojis := map[string]string{}
		var platformOrder []string
		for _, v := range missingMessaging {
			for _, row := range setupMessagingPlatforms {
				if !strings.Contains(v.Name, row[0]) {
					continue
				}
				if _, seen := grouped[row[1]]; !seen {
					platformOrder = append(platformOrder, row[1])
				}
				grouped[row[1]] = append(grouped[row[1]], v)
				emojis[row[1]] = row[2]
				break
			}
		}
		labels := make([]string, len(platformOrder))
		for i, p := range platformOrder {
			labels[i] = emojis[p] + " " + p
		}
		for _, idx := range provider.PromptChecklist("Which platforms would you like to set up?", labels, nil) {
			plat := platformOrder[idx]
			provider.SetupSectionRule(emojis[plat] + " " + plat)
			for _, v := range grouped[plat] {
				provider.SetupPrintInfo("  " + v.Description)
				if v.URL != "" {
					provider.SetupPrintInfo("  " + v.URL)
				}
				if err := promptAndSaveEnvVar(ctx, v, "  ✓ Saved", "  Skipped"); err != nil {
					return err
				}
				setupPrintln("")
			}
		}
	}
	if len(missingConfig) > 0 {
		provider.SetupInfo(nil, fmt.Sprintf("Adding %d new config option(s) with defaults...", len(missingConfig)))
		for _, field := range missingConfig {
			provider.SetupPrintSuccess(fmt.Sprintf("  Added %s = %s", field.Key, pyrepr.Str(field.Default)))
		}
		cfg["_config_version"] = latestVer
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return err
		}
	}
	return setupPrintSetupSummaryFn(ctx, cfg, icebathHome)
}
