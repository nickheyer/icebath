package whatsappcloud

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on WhatsApp (Meta Business Cloud API). Standard markdown auto-converts to WhatsApp syntax " +
	"— write markdown freely. No tables — use bullets or labeled lines. " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.jpg, .png) send as photos, videos (.mp4) inline, audio as voice/audio, other files as " +
	"documents; ![alt](url) works. NOTE: Meta refuses free-form replies when the user hasn't messaged in " +
	"24h (error 131047) — relevant only for delayed/scheduled sends."

// Dependency check, always met since net/http serves the webhook and the Graph client
func checkRequirements() bool { return true }

// Whether phone number id and access token are present in the config extra or the env
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if cfg != nil && pyrepr.Truthy(cfg.Extra["phone_number_id"]) && pyrepr.Truthy(cfg.Extra["access_token"]) {
		return true, nil
	}
	return gateway.EnvIsConnected("WHATSAPP_CLOUD_PHONE_NUMBER_ID", "WHATSAPP_CLOUD_ACCESS_TOKEN")(ctx, cfg)
}

// Gateway setup entry running the same wizard as icebath whatsapp-cloud on the setup console
func interactiveSetup(ctx context.Context) error {
	_, err := (&cloudWizard{ctx: ctx, home: home.Dir(ctx), console: provider.SetupConsole(), out: provider.SetupOutput()}).runWhatsAppCloudSetup()
	return err
}

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewWhatsAppCloudAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// WhatsApp Cloud platform plugin compiled into the binary under the bundled platforms/whatsapp_cloud key
type whatsAppCloudPlatformPlugin struct{}

func (whatsAppCloudPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "whatsapp_cloud-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/whatsapp_cloud",
		Author: "NousResearch",
		Description: "WhatsApp Business Cloud API adapter for Icebath Agent. Connects to WhatsApp via Meta's official Cloud API " +
			"(Business account and a public webhook URL required, token-based auth): Graph API for outbound, a net/http webhook " +
			"receiver with X-Hub-Signature-256 verification for inbound. Supports text, media (image/video/audio/document), voice " +
			"notes, interactive buttons, and DM allowlists. Complements the Baileys bridge (whatsapp) plugin; both share " +
			"WhatsAppBehaviorMixin.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "WHATSAPP_CLOUD_PHONE_NUMBER_ID", Description: "WhatsApp phone number ID from the Meta App Dashboard (WhatsApp → API Setup)"},
			{Name: "WHATSAPP_CLOUD_ACCESS_TOKEN", Description: "System User permanent access token", Secret: true}}}
}

// Registers the WhatsApp Cloud platform entry with its factory, setup wizard and dashboard card
func (whatsAppCloudPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"dashboard_label":        "WhatsApp Cloud API",
		"dashboard_description":  "Use Icebath via Meta's hosted WhatsApp Cloud API (no local bridge).",
		"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/whatsapp-cloud",
		"is_connected":           isConnected,
		"config_connected_fn":    gateway.ConfigConnectedExtra("phone_number_id", "access_token"),
		"setup_fn":               interactiveSetup,
		"cron_deliver_env_var":   "WHATSAPP_CLOUD_HOME_CHANNEL",
		"emoji":                  "📱",
		"allow_update_command":   false,
		"platform_hint":          platformHint,
		"display_tier":           "low",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "whatsapp_cloud", Label: "WhatsApp Business (Cloud)",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, cfg)
		}),
		Check:       checkRequirements,
		RequiredEnv: []string{"WHATSAPP_CLOUD_PHONE_NUMBER_ID", "WHATSAPP_CLOUD_ACCESS_TOKEN"},
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "WhatsAppCloudAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(whatsAppCloudPlatformPlugin{})
}
