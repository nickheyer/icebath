package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prints one line on the setup console
func setupPrintln(text string) { fmt.Fprintln(provider.SetupOutput(), text) }

// Wraps text in ANSI codes when the setup console takes color
func setupColor(text string, codes ...string) string {
	return provider.SetupConsole().Color(text, codes...)
}

// Prompts for one env value, masked when secret, saving it and confirming or reporting the skip
func promptAndSaveEnvVar(ctx context.Context, v config.MissingEnvVar, savedMsg, skippedMsg string) error {
	label := v.Prompt
	if label == "" {
		label = v.Name
	}
	value := provider.SetupPrompt("  "+label, "", v.Password)
	if value == "" {
		provider.SetupPrintWarning(skippedMsg)
		return nil
	}
	if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), v.Name, value); err != nil {
		return err
	}
	provider.SetupPrintSuccess(savedMsg)
	return nil
}

// Key entry screen for one tool env var, the tools it enables and where to get the key
func promptToolAPIKey(ctx context.Context, v config.MissingEnvVar) error {
	toolsText := strings.Join(v.Tools[:min(3, len(v.Tools))], ", ")
	if len(v.Tools) > 3 {
		toolsText += fmt.Sprintf(", +%d more", len(v.Tools)-3)
	}
	title := v.Description
	if title == "" {
		title = v.Name
	}
	provider.SetupSectionRule(title)
	if toolsText != "" {
		provider.SetupPrintInfo("  Enables: " + toolsText)
	}
	if v.URL != "" {
		provider.SetupPrintInfo("  Get your key at: " + v.URL)
	}
	setupPrintln("")
	return promptAndSaveEnvVar(ctx, v, "  ✓ Saved", "  Skipped (configure later with 'icebath setup')")
}

// One icebath setup section, the key a user names, its banner label and its runner
type setupSection struct {
	key, label string
	run        func(ctx context.Context, cfg config.Tree) error
}

// Runs the terminal backend section of pkg/tools in the setup tool environment
func setupTerminalBackendSection(ctx context.Context, cfg config.Tree) error {
	return tools.SetupTerminalBackend(ctx, provider.SetupToolsEnv(ctx), cfg)
}

// The independently runnable sections of icebath setup in menu order
var setupSections = []setupSection{
	{"model", "Model & Provider", provider.SetupModelProvider},
	{"tts", "Text-to-Speech", media.SetupTTS},
	{"terminal", "Terminal Backend", setupTerminalBackendSection},
	{"gateway", "Messaging Platforms (Gateway)", gateway.SetupGateway},
	{"tools", "Tools", func(ctx context.Context, cfg config.Tree) error { return provider.SetupTools(ctx, cfg, false) }},
	{"telemetry", "Shared Metrics", func(ctx context.Context, cfg config.Tree) error { return provider.SetupTelemetry(ctx, cfg) }},
	{"agent", "Agent Settings", provider.SetupAgentSettings},
}

// Seams over the sections the full and quick flows call, the hermes_cli.setup names the wizard tests patch
var (
	setupModelProviderFn      = provider.SetupModelProvider
	setupTerminalBackendFn    = setupTerminalBackendSection
	setupGatewayFn            = gateway.SetupGateway
	setupToolsFn              = provider.SetupTools
	setupOfferAgentImportFn   = provider.OfferAgentImport
	setupPrintSetupSummaryFn  = printSetupSummary
	setupRunQuickSetupFn      = runQuickSetup
	setupEnsureGatewayService = gateway.EnsureGatewayService
)

// RunSetupWizard runs icebath setup with Escape and Left arrow navigation scoped to this invocation
func RunSetupWizard(ctx context.Context, ns map[string]any) (code int, err error) {
	defer provider.WithSetupConsole(console())()
	defer provider.RecoverSetupExit(&code)
	err = provider.RunWithSetupNavigation(func() error { return runSetupWizardImpl(ctx, ns) },
		"Setup cancelled. Remaining sections were not changed.")
	if exitCode, message, exit := provider.SetupSystemExit(err); exit {
		if message != "" {
			fmt.Fprintln(stderrWriter(), message)
		}
		return exitCode, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Runs one named section under the banner, saving the config once it completes
func runSetupSection(ctx context.Context, cfg config.Tree, section string) error {
	var entry *setupSection
	for i := range setupSections {
		if setupSections[i].key == section {
			entry = &setupSections[i]
			break
		}
	}
	if entry == nil {
		provider.SetupPrintError("Unknown setup section: " + section)
		keys := make([]string, len(setupSections))
		for i, s := range setupSections {
			keys[i] = s.key
		}
		provider.SetupPrintInfo("Available sections: " + strings.Join(keys, ", "))
		return nil
	}
	provider.PrintSetupBanner("│     ☤ Icebath Setup — " + pystr.Ljust(entry.label, 33) + " │")
	if err := provider.RunSetupSteps([]provider.SetupStep{{Label: entry.label, Action: func() error { return entry.run(ctx, cfg) }}}); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{}); err != nil {
		return err
	}
	setupPrintln("")
	provider.SetupPrintSuccess(entry.label + " configuration complete!")
	return nil
}

// Runs every section, a section already configured by an import offering to keep its settings
func runFullSetup(ctx context.Context, cfg config.Tree, icebathHome string, isExisting bool, importedFrom string) error {
	provider.PrintSetupHeader("Configuration Location", false)
	provider.SetupInfo("Config file:  "+home.ConfigPath(icebathHome), "Secrets file: "+home.EnvPath(icebathHome),
		"Data folder:  "+icebathHome, "Install dir:  "+config.GetProjectRoot(), nil,
		"You can edit these files directly or use 'icebath config edit'")
	if importedFrom != "" {
		provider.SetupInfo(nil, "Settings were imported from "+importedFrom+".",
			"Each section below shows what is already set up — keep it,",
			"or choose to reconfigure it.")
	}
	if !isExisting {
		if err := provider.ApplyDefaultAgentSettings(ctx, cfg); err != nil {
			return err
		}
	}
	skip := func(key, label string) (bool, error) {
		if importedFrom == "" {
			return false, nil
		}
		return skipConfiguredSection(ctx, cfg, key, label)
	}
	step := func(key, label string, run func() error) provider.SetupStep {
		return provider.SetupStep{Label: label, Action: func() error {
			skipped, err := skip(key, label)
			if err != nil || skipped {
				return err
			}
			return run()
		}}
	}
	gatewayStep := func() error {
		skipped, err := skip("gateway", "Messaging Platforms")
		if err != nil {
			return err
		}
		if !skipped {
			return setupGatewayFn(ctx, cfg)
		}
		_, err = setupEnsureGatewayService(ctx, provider.SetupOutput(), gateway.ServiceSetupOptions{Context: "setup", Interactive: true, Config: cfg})
		return err
	}
	return provider.RunSetupSteps([]provider.SetupStep{
		step("model", "Model & Provider", func() error { return setupModelProviderFn(ctx, cfg) }),
		step("terminal", "Terminal Backend", func() error { return setupTerminalBackendFn(ctx, cfg) }),
		{Label: "Messaging Platforms", Action: gatewayStep},
		step("tools", "Tools", func() error { return setupToolsFn(ctx, cfg, !isExisting) }),
	})
}

// A first time setup mode, its menu label and the flow it runs, nil falling through to the full setup
type firstTimeMode struct {
	label string
	run   func(ctx context.Context, cfg config.Tree, icebathHome, importedFrom string) error
}

// Seams over the first time flows, the hermes_cli.setup_quick names the wizard tests patch
var (
	setupRunFirstTimeQuickSetupFn = runFirstTimeQuickSetup
	setupRunBlankSlateSetupFn     = runBlankSlateSetup
)

// The first time mode menu in order, Quick Setup the default
var firstTimeModes = []firstTimeMode{
	{"Quick Setup (Nous Portal) — free OAuth login, no API keys, model + tools (recommended)",
		func(ctx context.Context, cfg config.Tree, h, importedFrom string) error {
			return setupRunFirstTimeQuickSetupFn(ctx, cfg, h, importedFrom)
		}},
	{"Full setup — configure every provider, tool & option yourself (bring your own keys)", nil},
	{"Blank Slate — everything off except the bare minimum; opt in to each capability",
		func(ctx context.Context, cfg config.Tree, h, _ string) error {
			return setupRunBlankSlateSetupFn(ctx, cfg, h)
		}},
}

// Reports a truthy argparse attribute, absent attributes reading false
func setupFlag(ns map[string]any, key string) bool { return pyrepr.Truthy(ns[key]) }

// Whether a provider is configured, an OpenRouter key, a custom base URL or an auth store active provider
func setupIsExistingInstall(ctx context.Context) (bool, error) {
	for _, key := range []string{"OPENROUTER_API_KEY", "OPENAI_BASE_URL"} {
		v, err := setupEnvValue(ctx, key)
		if err != nil || v != "" {
			return err == nil, err
		}
	}
	data, err := provider.NewStore(home.Dir(ctx)).Load()
	if err != nil {
		return false, err
	}
	active, present := data["active_provider"]
	return present && active != nil, nil
}

// The setup wizard, full or quick by install state, the portal one shot, or one named section
func runSetupWizardImpl(ctx context.Context, ns map[string]any) error {
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "run setup wizard")
		return nil
	}
	if err := config.EnsureIcebathHome(ctx, h); err != nil {
		return err
	}
	configPath := home.ConfigPath(h)
	backupPath, err := config.BackupConfig(ctx, configPath, "pre-setup", config.DefaultBackupKeep)
	if err != nil {
		return err
	}
	if setupFlag(ns, "reset") {
		if err := config.SaveConfig(ctx, h, config.Defaults(), config.SaveOptions{}); err != nil {
			return err
		}
		provider.SetupPrintSuccess("Configuration reset to defaults.")
		if backupPath != "" {
			provider.SetupInfo("Previous config backed up to: " + backupPath)
		}
	}
	reconfigureRequested := setupFlag(ns, "reconfigure")
	quickRequested := setupFlag(ns, "quick")
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	if setupFlag(ns, "non_interactive") || !IsInteractiveStdin() {
		PrintNoninteractiveSetupGuidance("Running in a non-interactive environment (no TTY detected).")
		return nil
	}
	if setupFlag(ns, "portal") {
		return provider.RunPortalOneShot(ctx, cfg)
	}
	if section := ns["section"]; pyrepr.Truthy(section) {
		return runSetupSection(ctx, cfg, pyrepr.Str(section))
	}
	isExisting, err := setupIsExistingInstall(ctx)
	if err != nil {
		return err
	}
	provider.PrintSetupBanner("│             ☤ Icebath Agent Setup Wizard               │",
		"├─────────────────────────────────────────────────────────┤",
		"│  Let's configure your Icebath Agent installation.      │",
		"│  Press Ctrl+C at any time to exit.                     │")
	importedFrom := ""
	if isExisting {
		if quickRequested {
			if err := provider.RunSetupSteps([]provider.SetupStep{{Label: "Quick Setup", Action: func() error { return setupRunQuickSetupFn(ctx, cfg, h) }}}); err != nil {
				return err
			}
			return provider.RecordSetupCompleted(ctx, cfg)
		}
		provider.PrintSetupHeader("Reconfigure", true)
		provider.SetupPrintSuccess("You already have Icebath configured.")
		provider.SetupInfo("Running the full wizard — each prompt shows your current value.",
			"Press Enter to keep it, or type a new value to change it.", "",
			"Tip: jump straight to a section with 'icebath setup model|terminal|",
			"     gateway|tools|agent', or fill only missing items with --quick.")
	} else {
		setupPrintln("")
		if reconfigureRequested || quickRequested {
			provider.SetupInfo("No existing configuration found — running first-time setup.", nil)
		}
		if importedFrom, err = setupOfferAgentImportFn(ctx, h, true); err != nil {
			return err
		}
		if cfg, err = config.LoadContext(ctx, h, nil); err != nil {
			return err
		}
		labels := make([]string, len(firstTimeModes))
		for i, mode := range firstTimeModes {
			labels[i] = mode.label
		}
		mode := firstTimeModes[provider.PromptChoice("How would you like to set up Icebath?", labels, 0, "")]
		if mode.run != nil {
			if err := provider.RunSetupSteps([]provider.SetupStep{{Label: mode.label, Action: func() error { return mode.run(ctx, cfg, h, importedFrom) }}}); err != nil {
				return err
			}
			return provider.RecordSetupCompleted(ctx, cfg)
		}
	}
	if err := runFullSetup(ctx, cfg, h, isExisting, importedFrom); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	if backupPath != "" {
		if _, statErr := os.Stat(backupPath); statErr == nil {
			provider.SetupInfo("Previous config backed up to: "+backupPath,
				"If setup changed a value you customized, restore it with:",
				fmt.Sprintf("  cp %s %s", backupPath, configPath))
		}
	}
	if err := setupPrintSetupSummaryFn(ctx, cfg, h); err != nil {
		return err
	}
	return provider.RecordSetupCompleted(ctx, cfg)
}
