package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Setup surface the terminal UI reports provider setups under
const tuiSetupSurface = "tui"

// Picker rows a hosted model section may open straight at instead of the provider list
var hostedModelEntries = map[string]bool{"aux-config": true, "reasoning": true}

// Reports whether a model section can open straight at the named picker row or registered provider profile
func HostedModelEntry(entry string) bool {
	if entry == "" || hostedModelEntries[entry] || entry == "custom" || entry == "llamacpp" {
		return true
	}
	for _, p := range CanonicalProviders() {
		if p.Slug == entry {
			return true
		}
	}
	profile, ok := ProfileFor(entry)
	return ok && profile.Name == entry
}

// HostedModelResult is the default model and provider a hosted model section leaves, and whether it changed them
type HostedModelResult struct {
	Model    string
	Provider string
	Changed  bool
}

// Default model, provider and base URL of a config
func modelRoute(cfg config.Tree) [3]string {
	if model, ok := cfg["model"].(map[string]any); ok {
		route := [3]string{}
		route[0], _ = model["default"].(string)
		route[1], _ = model["provider"].(string)
		route[2], _ = model["base_url"].(string)
		return route
	}
	model, _ := cfg["model"].(string)
	return [3]string{model, "", ""}
}

// RunHostedModelSection runs the model picker on a host, opening at entry, then resaves the config unless stopped
func RunHostedModelSection(ctx context.Context, ask SetupAsker, out io.Writer, entry string) (HostedModelResult, error) {
	h := home.Dir(ctx)
	before, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return HostedModelResult{}, err
	}
	err = HostSetupFlow(ask, out, func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if _, exit := r.(SetupExit); !exit {
					panic(r)
				}
				SetupInfo("Provider setup skipped.")
				err = nil
			}
		}()
		if entry != "" {
			prev := pickProviderFn
			pickProviderFn = func(*ModelFlowRun, map[string]any, string, map[string]string, *customProviderMap) (string, bool, error) {
				pickProviderFn = prev
				return entry, true, nil
			}
			defer func() { pickProviderFn = prev }()
		}
		err = observability.ProviderSetupSurface(bindHostedPluginAsker(ctx, hostedAsker()), tuiSetupSurface, func(ctx context.Context) error {
			return RunSetupActionWithNavigation(ctx, "Model & Provider", func(ctx context.Context) error {
				return SelectProviderAndModel(ctx, nil)
			}, flowNoChange)
		})
		if _, _, exit := SetupSystemExit(err); err != nil && (exit || isSetupKeyboardInterrupt(err)) {
			SetupInfo("Provider setup skipped.")
			return nil
		}
		if err != nil {
			setupLog.Debug(fmt.Sprintf("select_provider_and_model error during tui setup: %v", err))
			lines := ProviderSetupFailureLines(err, "/setup model")
			SetupPrintWarning(lines[0])
			for _, line := range lines[1:] {
				SetupPrintInfo(line)
			}
		}
		return nil
	})
	if err != nil {
		return HostedModelResult{}, err
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return HostedModelResult{}, err
	}
	if ctx.Err() == nil {
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return HostedModelResult{}, err
		}
	}
	route := modelRoute(cfg)
	return HostedModelResult{Model: route[0], Provider: route[1], Changed: route != modelRoute(before)}, nil
}
