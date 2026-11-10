package provider

import (
	"context"
	"io"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seam over the consent question, the setup.prompt_yes_no the shared metrics tests patch
var setupTelemetryYesNo = PromptYesNo

// SwapSetupTelemetryPrompt swaps the shared metrics yes or no question, the returned func restoring it
func SwapSetupTelemetryPrompt(fn func(question string, def bool, description string) bool) func() {
	prev := setupTelemetryYesNo
	setupTelemetryYesNo = fn
	return func() { setupTelemetryYesNo = prev }
}

var sendConsentExplainer = []any{
	"", "Sharing uploads each daily package to the Nous telemetry",
	"service. Packages carry your profile-scoped install ID, a",
	"stable random UUID that identifies this profile across days",
	"(it contains no personal information and is reset by deleting",
	"the shared-metrics directory). Only packages whose entire",
	"collection period falls inside a recorded consent window are",
	"ever sent. Apart from the fresh-install note (noted on this",
	"machine and counted only once you opt in), data from before",
	"you opt in is never sent. Sharing can be turned off again at",
	"any time.",
}

// Turns shared metrics collection and sending on or off with one answer written to config.yaml at once
func SetupTelemetry(ctx context.Context, cfg map[string]any) error {
	PrintSetupHeader("Shared Metrics", false)
	SetupInfo("Shared metrics contain only bounded counters: activity, session length,",
		"outcomes, error classes (with a fixed-list reason when a memory write or",
		"context compression is refused, fails or is skipped), model routes and",
		"token totals, built-in tool, command and catalog names, bucketed setup",
		"counts, update and install results and timing (with a fixed-list reason",
		"and the stage when one fails; a fresh install is noted on this machine and",
		"counted only once you opt in), crashes,",
		"startup and reply speed, messaging-platform health, how Icebath gets used",
		"(agent accuracy and efficiency, active time per surface, which features and",
		"settings are used or switched off, provider setup outcomes), and coarse",
		"machine facts (RAM range, GPU type, version age and channel, updates behind,",
		"local model server yes/no). Never prompts, files, paths, setting values or",
		"error text.")
	SetupInfo(sendConsentExplainer...)
	shared := ensureDictSection(ensureDictSection(cfg, "telemetry"), "shared_metrics")
	sharing := shared["enabled"] == true && shared["send"] == true
	share := setupTelemetryYesNo("Send shared metrics to Nous?", sharing, "")
	if err := observability.SaveConsent(ctx, share, share, cfg); err != nil {
		return err
	}
	if share {
		setupUI().PrintSuccess("Shared metrics sent to Nous once a day.")
	} else {
		setupUI().PrintInfo("Shared metrics off.")
	}
	return nil
}

// Installs the provider entry points the tools command reaches through tools.ToolsCommandHooks
func init() {
	tools.ToolsCommandHooks.ApplyNousManagedDefaults = func(ctx context.Context, h string, cfg map[string]any, enabledToolsets []string, forceFresh bool) map[string]bool {
		env := homePoolEnv(h)
		env.Ctx = ctx
		return ApplyNousManagedDefaults(env, cfg, enabledToolsets, forceFresh)
	}
	tools.ToolsCommandHooks.VisionClientReady = func(ctx context.Context, h string) (bool, error) {
		_, client, _, err := ResolveVisionProviderClient(home.WithOverride(ctx, h), "", "", "", nil, false, nil)
		return err == nil && client != nil, err
	}
	tools.ToolsCommandHooks.SetupTelemetry = func(ctx context.Context, h string, out io.Writer, cfg map[string]any) error {
		defer WithSetupOutput(out)()
		return SetupTelemetry(home.WithOverride(ctx, h), cfg)
	}
}
