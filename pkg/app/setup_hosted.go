package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/setupflow"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Context key of the tool selector a hosted Blank Slate walkthrough opens instead of the terminal tools command
type hostedToolsPickerKey struct{}

// Positions of Quick Setup and Blank Slate in the first time mode menu
const (
	quickSetupMode = 0
	blankSlateMode = 2
)

// Sections and first run modes of icebath setup the terminal UI runs on the gateway, keyed by hosted flow
var HostedSetupFlows = map[string]func(ctx context.Context, ask setupflow.Asker) error{
	"gateway": hostedGatewaySection,
	"import":  hostedAgentImport,
	"quick":   hostedQuickSetup,
	"blank":   hostedBlankSlate,
}

// Runs body after the pre-setup backup and config load a setup run starts with, under the wizard navigation
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

// Messaging platforms section as icebath setup gateway runs it
func hostedGatewaySection(ctx context.Context, _ setupflow.Asker) error {
	return hostedSetupRun(ctx, func(_ string, cfg config.Tree) error { return runSetupSection(ctx, cfg, "gateway") })
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

// Quick Setup as icebath setup --quick runs it, the missing items of an existing install or a first setup
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
		return runFirstTimeMode(ctx, cfg, h, firstTimeModes[quickSetupMode], importedFrom)
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
		return runFirstTimeMode(ctx, cfg, h, firstTimeModes[blankSlateMode], importedFrom)
	})
}
