package homeassistant

import (
	"strings"

	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Explicit notify target of a reference, false for a blank one
func parseTargetRef(ref string) (string, *string, bool) {
	target := strings.TrimSpace(ref)
	return target, nil, target != ""
}

// Extra seeded from an env only setup, HASS_URL as the URL and HASS_HOME_CHANNEL as the cron home channel
func envEnablement(ctx context.Context) map[string]any {
	seed, err := gateway.SeedExtraFromEnv(ctx, []gateway.EnvSeedRow{{Env: "HASS_URL", Key: "url"}}, "HASS_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	return seed
}

// Home Assistant catalog plugin port, active once icebath plugins install homeassistant lands it in a home
type homeAssistantPlugin struct{}

func (homeAssistantPlugin) Manifest() plugins.Manifest {
	return plugins.CatalogPortManifest("homeassistant")
}

// Adapter class the gateway reads the message length trait from
var adapterClass = &gateway.AdapterClass{Name: "HomeAssistantAdapter", Traits: adapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewHomeAssistantAdapter(ctx, config)
	}}

// Registers the platform entry with its factory, gates, env seeding and cron sender, then the four tools
func (homeAssistantPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: platformName, Label: "Home Assistant",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewHomeAssistantAdapter(fctx, cfg)
		}),
		Check:       checkHARequirements,
		RequiredEnv: []string{"HASS_TOKEN"},
		InstallHint: "Home Assistant support is built into icebath — no extra deps",
		Extra: map[string]any{
			"scoped_validate_config": validateHAConfig,
			"is_connected":           gateway.EnvIsConnected("HASS_TOKEN"),
			"standalone_sender_fn":   standaloneSend,
			"max_message_length":     maxMessageLength,
			"emoji":                  toolEmoji,
			"allow_update_command":   true,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "HASS_HOME_CHANNEL",
			"parse_target_ref_fn":    parseTargetRef,
			"trusted_inbound":        true,
			"display_tier":           "minimal",
			"shared_env_prefixes":    []string{"HASS_"},
		},
	})
	if err != nil {
		return err
	}
	return registerTools(ctx)
}

// Registers the four smart home tools without the adapter, what discovery runs for the deferred platform
func (homeAssistantPlugin) RegisterTools(ctx *plugins.Context) error { return registerTools(ctx) }

var _ plugins.ToolsRegistrar = homeAssistantPlugin{}

func init() {
	gateway.RegisterAdapterModule(adapterClass)
	plugins.RegisterPort(homeAssistantPlugin{})
}
