package app

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Three line plus and dash box around a centered title
func infoBox(title string, width int) []string {
	pad := width - utf8.RuneCountInString(title)
	rule := "+" + strings.Repeat("-", width) + "+"
	return []string{rule, "|" + strings.Repeat(" ", max(0, pad/2)) + title + strings.Repeat(" ", max(0, pad-pad/2)) + "|", rule}
}

// Seams the reports read the toolset catalog through, bound to the default registry
var (
	allToolsetNames = func(ctx context.Context) []string {
		names := []string{}
		for name := range tools.Default.AllToolsets(ctx, "") {
			names = append(names, name)
		}
		return names
	}
	toolsetInfoOf = func(ctx context.Context, name string) (tools.ToolsetDetail, bool) {
		return tools.Default.ToolsetInfoFor(ctx, "", name)
	}
)

// The /toolsets report, every toolset with its tool count and the enabled ones starred
func toolsetsLines(ctx context.Context, enabled []string) []string {
	t := func(key string) string { return i18n.T(ctx, key, "", nil) }
	out := append(append([]string{""}, infoBox(t("cli.toolsets.header"), 58)...), "")
	names := allToolsetNames(ctx)
	sort.Strings(names)
	for _, name := range names {
		info, ok := toolsetInfoOf(ctx, name)
		if !ok {
			continue
		}
		marker := "   "
		for _, e := range enabled {
			if e == name {
				marker = "(*)"
				break
			}
		}
		out = append(out, pyFmt("  {marker} {name:<18} [{count:>2} tools] - {desc}",
			map[string]any{"marker": marker, "name": name, "count": info.ToolCount, "desc": info.Description}))
	}
	return append(out, "", "  "+t("cli.toolsets.currently_enabled_legend"), "", "  "+t("cli.toolsets.tip_enable_all"), "  "+t("cli.toolsets.example"), "")
}

// The /platforms report, the messaging platforms the profile's gateway config enables or the setup steps on a load failure
func platformsLines(ctx context.Context) []string {
	t := func(key string, kw map[string]any) string { return i18n.T(ctx, key, "", kw) }
	out := append(append([]string{""}, infoBox(t("cli.gateway_status.header", nil), 60)...), "")
	configFile := home.Display(home.Dir(ctx)) + "/config.yaml"
	var body []string
	err := pyExcept(func() error {
		cfg, err := loadGatewayConfig(ctx)
		if err != nil {
			return err
		}
		body = platformsReport(ctx, cfg, configFile)
		return nil
	})
	if err != nil {
		return append(out, "  "+t("cli.gateway_status.load_error", map[string]any{"error": err.Error()}), "",
			"  "+t("cli.gateway_status.to_configure", nil), "    "+t("cli.gateway_status.step_env_vars", nil),
			"       TELEGRAM_BOT_TOKEN=your_token", "       DISCORD_BOT_TOKEN=your_token",
			"    "+t("cli.gateway_status.step_config", map[string]any{"path": configFile}), "")
	}
	return append(out, body...)
}

// Platform rows and the start hints of a loaded gateway config
func platformsReport(ctx context.Context, cfg *gateway.GatewayConfig, configFile string) []string {
	t := func(key string, kw map[string]any) string { return i18n.T(ctx, key, "", kw) }
	out := []string{"  " + t("cli.gateway_status.platform_config_header", nil), "  " + strings.Repeat("-", 55)}
	for _, p := range []struct {
		platform     gateway.Platform
		name, envVar string
	}{
		{gateway.PlatformTelegram, "Telegram", "TELEGRAM_BOT_TOKEN"},
		{gateway.PlatformDiscord, "Discord", "DISCORD_BOT_TOKEN"},
		{gateway.PlatformSlack, "Slack", "SLACK_BOT_TOKEN"},
		{gateway.PlatformWhatsApp, "WhatsApp", "WHATSAPP_ENABLED"},
	} {
		if pc := cfg.Platforms[p.platform]; pc != nil && pc.Enabled {
			homeStr := ""
			if h := cfg.GetHomeChannel(p.platform); h != nil {
				homeStr = " → " + pyrepr.Str(h.Name)
			}
			out = append(out, pyFmt("    ✓ {name:<12} ", map[string]any{"name": p.name})+t("cli.gateway_status.enabled", nil)+homeStr)
		} else {
			out = append(out, pyFmt("    ○ {name:<12} ", map[string]any{"name": p.name})+t("cli.gateway_status.not_configured", map[string]any{"env_var": p.envVar}))
		}
	}
	return append(out, "", "  "+t("cli.gateway_status.conversations_persist", nil), "", "  "+t("cli.gateway_status.to_start", nil),
		"    icebath gateway run", "", "  "+t("cli.gateway_status.config_file", map[string]any{"path": configFile}), "")
}

// Gateway config loader of the /platforms report, a raised panic turned into the load error, swapped by tests
var loadGatewayConfig = func(ctx context.Context) (cfg *gateway.GatewayConfig, err error) {
	err = pyExcept(func() (err error) {
		cfg, err = gateway.LoadGatewayConfig(ctx)
		return err
	})
	return cfg, err
}
