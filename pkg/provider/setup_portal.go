package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Seam over the Nous Portal flow, the hermes_cli.model_setup_flows._model_flow_nous the setup tests patch
var setupModelFlowNous = ModelFlowNous

// SwapSetupModelFlowNous replaces the Nous Portal flow of the setup wizard, the returned func restoring it
func SwapSetupModelFlowNous(fn func(ctx context.Context, cfg map[string]any) error) func() {
	prev := setupModelFlowNous
	setupModelFlowNous = fn
	return func() { setupModelFlowNous = prev }
}

// PrintSetupBanner prints the magenta box banner, top border, the body lines and bottom border
func PrintSetupBanner(lines ...string) {
	c := setupUI()
	setupPrintln("")
	setupPrintln(c.Color("┌─────────────────────────────────────────────────────────┐", cliui.Magenta))
	for _, line := range lines {
		setupPrintln(c.Color(line, cliui.Magenta))
	}
	setupPrintln(c.Color("└─────────────────────────────────────────────────────────┘", cliui.Magenta))
}

// Counts a finished wizard run, the one time shared metrics offer asked first so an opt in is counted
func recordSetupCompleted(ctx context.Context, cfg map[string]any) error {
	if err := observability.OfferConsentIfUndecided(ctx, cfg); err != nil {
		return err
	}
	var providerName any
	if model, ok := cfg["model"].(map[string]any); ok {
		providerName = model["provider"]
	}
	observability.RecordSetupCompleted(ctx, "cli", providerName)
	return nil
}

// Seam over the finished run counter, the hermes_cli.setup._record_setup_completed the setup tests patch
var setupRecordCompleted = recordSetupCompleted

// RecordSetupCompleted counts a finished wizard run, every setup flow ending here
func RecordSetupCompleted(ctx context.Context, cfg map[string]any) error {
	return setupRecordCompleted(ctx, cfg)
}

// SwapSetupRecordCompleted replaces the finished run counter of the setup flows, the returned func restoring it
func SwapSetupRecordCompleted(fn func(ctx context.Context, cfg map[string]any) error) func() {
	prev := setupRecordCompleted
	setupRecordCompleted = fn
	return func() { setupRecordCompleted = prev }
}

// Reports a KeyboardInterrupt or EOFError leaving a prompt or a login wait
func setupInterruptedOrEOF(err error) bool {
	return errors.Is(err, cliui.ErrEOF) || isSetupKeyboardInterrupt(err)
}

// RunNousFlow runs the Nous Portal login and model pick, false when cancelled or failed with the message printed
func RunNousFlow(ctx context.Context, cfg map[string]any, flowContext string, cancelOnExit bool, cancelLines []any, printError func(error)) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, exit := r.(SetupExit); exit && cancelOnExit {
				SetupInfo(cancelLines...)
				ok, err = false, nil
				return
			}
			panic(r)
		}
	}()
	ferr := setupModelFlowNous(ctx, cfg)
	if ferr == nil {
		return true, nil
	}
	_, _, isExit := SetupSystemExit(ferr)
	switch {
	case isExit && !cancelOnExit:
		return false, ferr
	case isExit || setupInterruptedOrEOF(ferr):
		SetupInfo(cancelLines...)
		return false, nil
	}
	setupLog.Debug(fmt.Sprintf("_model_flow_nous error during %s: %v", flowContext, ferr))
	printError(ferr)
	return false, nil
}

// RunPortalOneShot is the one shot Nous Portal setup of icebath setup --portal and icebath portal
func RunPortalOneShot(ctx context.Context, cfg config.Tree) error {
	PrintSetupBanner("│     ☤ Icebath Setup — Nous Portal (one-shot)            │")
	SetupInfo(nil, "  One subscription, 300+ models, plus the Tool Gateway:",
		"    web search, image generation, TTS, browser automation",
		"    — all routed through your Nous Portal sub.", nil,
		"  Sign up: https://portal.nousresearch.com/manage-subscription", nil)
	onError := func(err error) {
		setupPrintln("")
		lines := ProviderSetupFailureLines(err, "icebath portal")
		SetupPrintError("  " + lines[0])
		for _, line := range lines[1:] {
			SetupPrintInfo("  " + line)
		}
	}
	ok, err := RunNousFlow(ctx, cfg, "`icebath portal`", true,
		[]any{nil, "  Setup cancelled.", "  You can retry later with `icebath portal`."}, onError)
	if err != nil || !ok {
		return err
	}
	_ = ReloadSetupConfig(ctx, cfg, false)
	setupPrintln("")
	SetupPrintSuccess("Portal setup complete.")
	SetupInfo("  Run `icebath portal info` to inspect routing.", "  Run `icebath` to start chatting.")
	return RecordSetupCompleted(ctx, cfg)
}
