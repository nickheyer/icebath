package whatsapp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on WhatsApp. Standard markdown auto-converts to WhatsApp syntax (*bold*, _italic_, ~strike~, " +
	"monospace) — write markdown freely, bullets included. No tables — use bullets or labeled lines. " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png, .webp) send as photos, videos (.mp4, .mov) play " +
	"inline, other files arrive as documents; image URLs via ![alt](url) send as photos."

// WhatsApp JIDs the adapter accepts verbatim as explicit send targets
const sendJIDPattern = `(?i)^\s*[\w-]+@(?:g\.us|s\.whatsapp\.net|lid|broadcast|newsletter)\s*$`

// Whether the dependencies of the adapter are present, always since the WhatsApp Web client is compiled in
func checkWhatsAppRequirements() bool { return true }

// Connected when an enabled config carries extras, else when WHATSAPP_ENABLED opts in
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if cfg != nil && cfg.Enabled && len(cfg.Extra) > 0 {
		return true, nil
	}
	value, _, err := config.GetEnvValue(ctx, home.Dir(ctx), "WHATSAPP_ENABLED")
	if err != nil {
		return false, err
	}
	switch pystr.Lower(pystr.Strip(value)) {
	case "true", "1", "yes":
		return true, nil
	}
	return false, nil
}

// Guides the user through enabling WhatsApp, its allowlist and its home chat
func interactiveSetup(ctx context.Context) error {
	con := cliui.Default()
	h := home.Dir(ctx)
	con.PrintHeader("WhatsApp")
	con.PrintInfo("WhatsApp uses the built-in WhatsApp Web client, paired once by QR code.")
	con.PrintInfo("Pair it with `icebath whatsapp`; the gateway runs the client in process.")
	enabled, _, err := config.GetEnvValue(ctx, h, "WHATSAPP_ENABLED")
	if err != nil {
		return err
	}
	if v := pystr.Lower(enabled); v == "true" || v == "1" || v == "yes" {
		con.PrintInfo("WhatsApp: already enabled")
		if !con.PromptYesNo(ctx, "Reconfigure WhatsApp?", false) {
			return nil
		}
	}
	if !con.PromptYesNo(ctx, "Enable WhatsApp?", true) {
		if err := config.SaveEnvValue(h, "WHATSAPP_ENABLED", "false"); err != nil {
			return err
		}
		con.PrintInfo("WhatsApp left disabled")
		return nil
	}
	if err := config.SaveEnvValue(h, "WHATSAPP_ENABLED", "true"); err != nil {
		return err
	}
	con.PrintSuccess("WhatsApp enabled")
	if allowed := con.Prompt("Allowed user IDs (comma-separated, leave empty for no allowlist)", "", false); allowed != "" {
		if err := config.SaveEnvValue(h, "WHATSAPP_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
		con.PrintSuccess("WhatsApp allowlist configured")
	}
	if homeChannel := pystr.Strip(con.Prompt("Home chat ID for cron delivery (leave empty to skip)", "", false)); homeChannel != "" {
		return config.SaveEnvValue(h, "WHATSAPP_HOME_CHANNEL", homeChannel)
	}
	removed, err := config.RemoveEnvValue(filepath.Join(h, ".env"), "WHATSAPP_HOME_CHANNEL")
	if err != nil {
		return err
	}
	if removed {
		con.PrintInfo("Home channel cleared.")
	}
	return nil
}

// Bundled WhatsApp platform plugin registering the adapter with the gateway
type whatsappPlugin struct{}

func (whatsappPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "whatsapp-platform", Key: "platforms/whatsapp", Kind: "platform", Version: "1.0.0", Author: "NousResearch",
		Description: "WhatsApp gateway adapter for Icebath Agent. Connects to WhatsApp through the built-in WhatsApp Web client and relays " +
			"messages between WhatsApp chats and the Icebath agent. Supports DM/group policies, mention gating, free-response chats, and per-user allowlists.",
		RequiresEnv: []plugins.EnvSpec{{Name: "WHATSAPP_ENABLED", Description: "Enable the WhatsApp adapter"}}}
}

// Registers the WhatsApp platform with its sender, YAML bridge, setup and access policy declarations
func (whatsappPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn": gateway.RegexStrippedParser(sendJIDPattern),
		"standalone_send": &gateway.StandaloneSendPolicy{Label: "WhatsApp", Media: true, Captionable: true,
			ForwardForceDocument: true, Mentions: true, TextDirect: true},
		"dashboard_description":  "Use Icebath through the bundled WhatsApp bridge with QR-based auth.",
		"docs_url":               "https://github.com/tulir/whatsmeow",
		"dashboard_env_vars":     []string{"WHATSAPP_ENABLED", "WHATSAPP_MODE", "WHATSAPP_DM_POLICY", "WHATSAPP_ALLOWED_USERS"},
		"dashboard_required_env": []string{},
		"is_connected":           isConnected,
		"setup_fn":               interactiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "WHATSAPP_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     whatsappMaxMessageLength,
		"emoji":                  "\U0001F4AC",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "medium",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{Name: "whatsapp", Label: "WhatsApp",
		AdapterFactory: gateway.AdapterFactory(func(c context.Context, cfg *gateway.PlatformConfig) (any, error) { return NewWhatsAppAdapter(c, cfg) }),
		Check:          checkWhatsAppRequirements, RequiredEnv: []string{"WHATSAPP_ENABLED"},
		InstallHint: "WhatsApp pairs through the built-in WhatsApp Web client — run `icebath whatsapp`", Extra: extra})
	return err
}

// Adds the WhatsApp platform plugin to the compiled-in set
func init() { plugins.RegisterBundled(whatsappPlugin{}) }
