package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/setupflow"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Context key of the tool selector a hosted Blank Slate walkthrough opens
type hostedToolsPickerKey struct{}

// Setup flows the terminal UI runs on the gateway, keyed by hosted flow
var HostedSetupFlows = map[string]func(ctx context.Context, ask setupflow.Asker) error{
	"gateway": hostedGatewaySection,
	"import":  hostedAgentImport,
	"quick":   hostedQuickSetup,
	"blank":   hostedBlankSlate,
}

// Prints one line on the setup console
func setupPrintln(text string) { fmt.Fprintln(provider.SetupOutput(), text) }

// Wraps text in ANSI codes when the setup console takes color
func setupColor(text string, codes ...string) string {
	return provider.SetupConsole().Color(text, codes...)
}

// Value of an env name the setup flows read, empty when unset
func setupEnvValue(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
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
	return promptAndSaveEnvVar(ctx, v, "  ✓ Saved", "  Skipped (configure later with '/setup')")
}

// Runs the terminal backend section of pkg/tools in the setup tool environment
func setupTerminalBackendSection(ctx context.Context, cfg config.Tree) error {
	return tools.SetupTerminalBackend(ctx, provider.SetupToolsEnv(ctx), cfg)
}

// Seams over the sections the quick and Blank Slate flows call, swapped out by the setup tests
var (
	setupModelProviderFn      = provider.SetupModelProvider
	setupTerminalBackendFn    = setupTerminalBackendSection
	setupGatewayFn            = gateway.SetupGateway
	setupOfferAgentImportFn   = provider.OfferAgentImport
	setupPrintSetupSummaryFn  = printSetupSummary
	setupRunQuickSetupFn      = runQuickSetup
	setupEnsureGatewayService = gateway.EnsureGatewayService
)

// Label of the messaging platforms section
const gatewaySectionLabel = "Messaging Platforms (Gateway)"

// Runs the messaging platforms section under its banner, saving the config once it completes
func runGatewaySection(ctx context.Context, cfg config.Tree) error {
	provider.PrintSetupBanner("│     ☤ Icebath Setup — " + pystr.Ljust(gatewaySectionLabel, 33) + " │")
	if err := provider.RunSetupSteps([]provider.SetupStep{{Label: gatewaySectionLabel, Action: func() error { return setupGatewayFn(ctx, cfg) }}}); err != nil {
		return err
	}
	if err := config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{}); err != nil {
		return err
	}
	setupPrintln("")
	provider.SetupPrintSuccess(gatewaySectionLabel + " configuration complete!")
	return nil
}

// A first time setup mode, its step label and the flow it runs
type firstTimeMode struct {
	label string
	run   func(ctx context.Context, cfg config.Tree, icebathHome, importedFrom string) error
}

// Seams over the first time flows, swapped out by the setup tests
var (
	setupRunFirstTimeQuickSetupFn = runFirstTimeQuickSetup
	setupRunBlankSlateSetupFn     = runBlankSlateSetup
)

// Quick Setup keeping an imported model, with default agent settings, terminal and messaging when the build links platforms
func quickSetupMode() firstTimeMode {
	label := "Quick Setup — keeps an imported model, default agent settings, terminal + messaging (recommended)"
	if !linktier.Linked(linktier.Plugins) {
		label = "Quick Setup — keeps an imported model, default agent settings and terminal (recommended)"
	}
	return firstTimeMode{label, func(ctx context.Context, cfg config.Tree, h, importedFrom string) error {
		return setupRunFirstTimeQuickSetupFn(ctx, cfg, h, importedFrom)
	}}
}

// Blank Slate with everything off except the bare minimum
var blankSlateMode = firstTimeMode{"Blank Slate — everything off except the bare minimum; opt in to each capability",
	func(ctx context.Context, cfg config.Tree, h, _ string) error {
		return setupRunBlankSlateSetupFn(ctx, cfg, h)
	}}

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

// Message a setup run prints when navigation backs out of it
const setupCancelledMessage = "Setup cancelled. Remaining sections were not changed."

// Ensures the home and takes the pre-setup backup a setup run starts with, empty when no config was there
func setupBackup(ctx context.Context, h string) (string, error) {
	if err := config.EnsureIcebathHome(ctx, h); err != nil {
		return "", err
	}
	return config.BackupConfig(ctx, home.ConfigPath(h), "pre-setup", config.DefaultBackupKeep)
}

// Prints the title box of a whole setup run
func printSetupWizardTitle() {
	provider.PrintSetupBanner("│             ☤ Icebath Agent Setup Wizard               │",
		"├─────────────────────────────────────────────────────────┤",
		"│  Let's configure your Icebath Agent installation.      │",
		"│  Press Ctrl+C at any time to exit.                     │")
}

// Quick Setup of an existing install as one step
func runExistingQuickSetup(ctx context.Context, cfg config.Tree, h string) error {
	return provider.RunSetupSteps([]provider.SetupStep{{Label: "Quick Setup", Action: func() error { return setupRunQuickSetupFn(ctx, cfg, h) }}})
}

// Offers the agent import of a first setup, returning the config it left and the agents it imported
func offerFirstSetupImport(ctx context.Context, h string) (config.Tree, string, error) {
	importedFrom, err := setupOfferAgentImportFn(ctx, h, true)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	return cfg, importedFrom, err
}

// Runs a first time mode as one step
func runFirstTimeMode(ctx context.Context, cfg config.Tree, h string, mode firstTimeMode, importedFrom string) error {
	return provider.RunSetupSteps([]provider.SetupStep{{Label: mode.label, Action: func() error { return mode.run(ctx, cfg, h, importedFrom) }}})
}

// Runs body after the pre-setup backup and config load a setup run starts with, under the setup navigation
func hostedSetupRun(ctx context.Context, body func(h string, cfg config.Tree) error) error {
	h := home.Dir(ctx)
	if _, err := setupBackup(ctx, h); err != nil {
		return err
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	return provider.RunWizardWithSetupNavigation(func() error { return body(h, cfg) }, setupCancelledMessage)
}

// Messaging platforms section with its banner and the config save it ends with
func hostedGatewaySection(ctx context.Context, _ setupflow.Asker) error {
	return hostedSetupRun(ctx, func(_ string, cfg config.Tree) error { return runGatewaySection(ctx, cfg) })
}

// Agent import step of a first setup, or the import hint icebath migrate prints when no other agent is found
func hostedAgentImport(ctx context.Context, _ setupflow.Asker) error {
	if len(provider.DetectImportSources()) == 0 || provider.IsNoninteractive() {
		_, err := provider.MigrateFromAgents(ctx, home.Dir(ctx), "", provider.AgentImportOptions{})
		return err
	}
	return hostedSetupRun(ctx, func(h string, _ config.Tree) error {
		_, err := setupOfferAgentImportFn(ctx, h, true)
		return err
	})
}

// Quick Setup, the missing items of an existing install or a first setup
func hostedQuickSetup(ctx context.Context, _ setupflow.Asker) error {
	return hostedSetupRun(ctx, func(h string, cfg config.Tree) error {
		isExisting, err := setupIsExistingInstall(ctx)
		if err != nil {
			return err
		}
		printSetupWizardTitle()
		if isExisting {
			return runExistingQuickSetup(ctx, cfg, h)
		}
		setupPrintln("")
		provider.SetupInfo("No existing configuration found — running first-time setup.", nil)
		cfg, importedFrom, err := offerFirstSetupImport(ctx, h)
		if err != nil {
			return err
		}
		return runFirstTimeMode(ctx, cfg, h, quickSetupMode(), importedFrom)
	})
}

// Blank Slate after the import offer of a first setup, its tool selector the tools section asked through the host
func hostedBlankSlate(ctx context.Context, ask setupflow.Asker) error {
	ctx = context.WithValue(ctx, hostedToolsPickerKey{}, func(ctx context.Context) error {
		return tuiserver.RunToolsSetupFlow(ctx, home.Dir(ctx), ask)
	})
	return hostedSetupRun(ctx, func(h string, cfg config.Tree) error {
		isExisting, err := setupIsExistingInstall(ctx)
		if err != nil {
			return err
		}
		printSetupWizardTitle()
		importedFrom := ""
		if !isExisting {
			setupPrintln("")
			if cfg, importedFrom, err = offerFirstSetupImport(ctx, h); err != nil {
				return err
			}
		}
		return runFirstTimeMode(ctx, cfg, h, blankSlateMode, importedFrom)
	})
}
