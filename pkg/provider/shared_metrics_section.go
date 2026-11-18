package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// SharedMetricsSectionConsent asks whether shared metrics go to Nous and saves the answer to both consent keys of cfg
func SharedMetricsSectionConsent(ctx context.Context, cfg map[string]any, ask setupflow.Asker) error {
	for _, line := range []string{"Shared Metrics",
		"Bounded counters only, activity, session length, outcomes and error classes, model routes and token totals,",
		"built-in tool, command and catalog names, setup, update and install results, crashes, startup and reply speed,",
		"messaging platform health, feature use and coarse machine facts. Never prompts, files, paths, setting values or error text.",
		"Sharing uploads one package a day under a random install ID of this profile, only days inside your consent are sent."} {
		ask.Say(line)
	}
	telemetry, _ := cfg["telemetry"].(map[string]any)
	shared, _ := telemetry["shared_metrics"].(map[string]any)
	def := 1
	if shared["enabled"] == true && shared["send"] == true {
		def = 0
	}
	idx, err := ask.Choose("Send shared metrics to Nous?", []setupflow.Option{{Title: "Yes"}, {Title: "No"}}, def)
	if err != nil {
		return err
	}
	share := idx == 0
	if err := observability.SaveConsent(ctx, share, share, cfg); err != nil {
		return err
	}
	if share {
		ask.Say("✓ Shared metrics sent to Nous once a day.")
	} else {
		ask.Say("  Shared metrics off.")
	}
	return nil
}
