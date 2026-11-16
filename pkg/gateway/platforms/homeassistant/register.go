package homeassistant

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const pluginDescription = "Home Assistant for Icebath Agent: a gateway platform adapter plus four " +
	"smart-home tools. The adapter subscribes to HA's WebSocket event bus and " +
	"forwards state-change events (with per-entity cooldowns and domain/entity " +
	"filtering) to the agent; replies are delivered as HA persistent " +
	"notifications, and cron deliver=homeassistant goes through the " +
	"target's notify service (notify.<target>). The homeassistant toolset (ha_list_entities, " +
	"ha_get_state, ha_list_services, ha_call_service) queries and controls " +
	"devices over the REST API.\n"

// Explicit notify target of a reference, false for a blank one
func parseTargetRef(ref string) (string, *string, bool) {
	target := pystr.Strip(ref)
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

// Home Assistant platform plugin compiled into the binary under the bundled platforms/homeassistant key
type homeAssistantPlugin struct{}

func (homeAssistantPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "homeassistant", Version: "2.0.1", Kind: "platform", Source: "bundled", Key: "platforms/homeassistant",
		Author: "NousResearch", Description: pluginDescription,
		ProvidesTools: []string{"ha_list_entities", "ha_get_state", "ha_list_services", "ha_call_service"},
		RequiresEnv:   []plugins.EnvSpec{{Name: "HASS_TOKEN", Description: "Home Assistant Long-Lived Access Token", Secret: true}}}
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
	plugins.RegisterBundled(homeAssistantPlugin{})
}
