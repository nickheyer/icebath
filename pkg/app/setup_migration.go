package app

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seams over the gateway platform table, the hermes_cli.gateway names the migration tests patch
var (
	setupAllPlatforms   = gateway.AllPlatforms
	setupPlatformStatus = gateway.PlatformStatus
)

// Value of an env name the setup flows read, empty when unset
func setupEnvValue(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// Model shown for an already configured model section, empty when no credentials are usable
func setupModelSummary(ctx context.Context, cfg config.Tree) (string, error) {
	ok, err := provider.ModelSectionHasCredentials(ctx, cfg)
	if err != nil || !ok {
		return "", err
	}
	switch model := cfg["model"].(type) {
	case string:
		if stripped := pystr.Strip(model); stripped != "" {
			return stripped, nil
		}
	case map[string]any:
		for _, key := range []string{"default", "model"} {
			if v := model[key]; pyrepr.Truthy(v) {
				return pyrepr.Str(v), nil
			}
		}
	}
	return "configured", nil
}

// Prefixed value of one config key, its default when the key is missing
func setupCfgSummary(cfg config.Tree, section, key string, def any, prefix string) string {
	v, ok := config.CfgGet(cfg, section, key)
	if !ok {
		v = def
	}
	return prefix + pyrepr.Str(v)
}

// Labels of the messaging platforms whose setup has started, empty when none
func setupGatewaySummary(ctx context.Context) (string, error) {
	var configured []string
	for _, plat := range setupAllPlatforms(ctx) {
		status, err := setupPlatformStatus(ctx, plat)
		if err != nil {
			return "", err
		}
		if status == "" {
			continue
		}
		if status, err = setupPlatformStatus(ctx, plat); err != nil {
			return "", err
		}
		if status == "not configured" {
			continue
		}
		label := pystr.Strip(strings.SplitN(plat.Label, "(", 2)[0])
		if label == "" {
			label = plat.Label
		}
		configured = append(configured, label)
	}
	return strings.Join(configured, ", "), nil
}

// Tool env names whose key is set and the labels they summarize as
var setupToolEnvLabels = [][2]string{
	{"ELEVENLABS_API_KEY", "TTS/ElevenLabs"},
	{"BROWSERBASE_API_KEY", "Browser"},
	{"FIRECRAWL_API_KEY", "Firecrawl"},
}

// Labels of the tool keys already set, empty when none
func setupToolsSummary(ctx context.Context) (string, error) {
	var tools []string
	for _, row := range setupToolEnvLabels {
		v, err := setupEnvValue(ctx, row[0])
		if err != nil {
			return "", err
		}
		if v != "" {
			tools = append(tools, row[1])
		}
	}
	return strings.Join(tools, ", "), nil
}

// Short summary of an already configured setup section, empty when it is not configured
func getSectionConfigSummary(ctx context.Context, cfg config.Tree, sectionKey string) (string, error) {
	switch sectionKey {
	case "model":
		return setupModelSummary(ctx, cfg)
	case "terminal":
		return setupCfgSummary(cfg, "terminal", "backend", "local", "backend: "), nil
	case "agent":
		return setupCfgSummary(cfg, "agent", "max_turns", 90, "max turns: "), nil
	case "gateway":
		return setupGatewaySummary(ctx)
	case "tools":
		return setupToolsSummary(ctx)
	}
	return "", nil
}

// Shows a configured section's summary and offers to skip it, true when the user keeps it as is
func skipConfiguredSection(ctx context.Context, cfg config.Tree, sectionKey, label string) (bool, error) {
	summary, err := getSectionConfigSummary(ctx, cfg, sectionKey)
	if err != nil || summary == "" {
		return false, err
	}
	setupPrintln("")
	provider.SetupPrintSuccess("  " + label + ": " + summary)
	return !provider.PromptYesNo("  Reconfigure "+pystr.Lower(label)+"?", false, ""), nil
}
