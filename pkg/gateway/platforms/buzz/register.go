package buzz

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Buzz channel and DM ids, native send targets taken verbatim
var sendUUIDRE = regexp.MustCompile(`(?i)^[` + wsClass + `]*[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}[` + wsClass + `]*$`)

// YAML key, env var and encoding kind of every buzz block key bridged into the environment
var yamlBridge = []gateway.YAMLBridgeRow{
	{Key: "relay_url", Env: "BUZZ_RELAY_URL", Kind: "str"}, {Key: "cli_path", Env: "BUZZ_CLI_PATH", Kind: "str"},
	{Key: "home_channel", Env: "BUZZ_HOME_CHANNEL", Kind: "str"}, {Key: "transport", Env: "BUZZ_TRANSPORT", Kind: "str"},
	{Key: "poll_interval", Env: "BUZZ_POLL_INTERVAL", Kind: "str"},
	{Key: "channels", Env: "BUZZ_CHANNELS", Kind: "csv"}, {Key: "allowed_users", Env: "BUZZ_ALLOWED_USERS", Kind: "csv"},
	{Key: "reaction_only_users", Env: "BUZZ_REACTION_ONLY_USERS", Kind: "csv"},
	{Key: "allow_all_users", Env: "BUZZ_ALLOW_ALL_USERS", Kind: "lower"},
	{Key: "require_mention", Env: "BUZZ_REQUIRE_MENTION", Kind: "lower"},
	{Key: "reply_in_thread", Env: "BUZZ_REPLY_IN_THREAD", Kind: "lower"},
	{Key: "reply_to_mode", Env: "BUZZ_REPLY_TO_MODE", Kind: "lower"},
}

// Model facing description of the Buzz workspace
const platformHint = "You are collaborating in a Buzz workspace (Block's Nostr-based " +
	"human+agent platform). Markdown IS supported. Users address you " +
	"by @-mentioning your name or npub in channels; direct messages " +
	"reach you without a mention. Keep responses conversational."

// Stripped send target when the whole reference is a Buzz id, nil for anything else
func parseSendTarget(ref string) any {
	if !sendUUIDRE.MatchString(ref) {
		return nil
	}
	chatID := strip(ref)
	return &gateway.TargetRefParse{ChatID: &chatID}
}

// Bridges buzz YAML keys into BUZZ env vars, explicit env winning, and returns the extra they seed
func applyYAMLConfig(ctx context.Context, _ map[string]any, buzzCfg map[string]any) (map[string]any, error) {
	var raw any = buzzCfg
	if v, ok := buzzCfg["extra"]; ok {
		raw = v
	}
	if !truthy(raw) {
		raw = map[string]any{}
	}
	extra, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	return gateway.ApplyYAMLBridge(ctx, extra, yamlBridge)
}

// Buzz extra of the scoped profile's own config.yaml, empty outside a secondary profile or when unreadable
func profileBuzzExtra(ctx context.Context) map[string]any {
	if !gateway.ProfileScoped(ctx) {
		return map[string]any{}
	}
	cfg, err := config.ReadRawConfigReadonly(ctx, home.Dir(ctx))
	if err != nil || cfg == nil {
		return map[string]any{}
	}
	var gatewayPlatforms any
	if gw, ok := cfg["gateway"].(map[string]any); ok {
		gatewayPlatforms = gw["platforms"]
	}
	section, _ := gateway.PlatformSection(cfg, "buzz", gatewayPlatforms)
	block, ok := section.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if inner, has := block["extra"]; has {
		extra, ok := inner.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		return extra
	}
	return block
}

// Whether a relay URL and a resolvable private key are configured for the caller's profile
func checkRequirements(ctx context.Context) bool {
	var relay, key string
	var err error
	if gateway.ProfileScoped(ctx) {
		s := &settings{ctx: ctx, extra: profileBuzzExtra(ctx)}
		relay, key, err = s.relayURL(), s.privateKey(), s.err
	} else {
		s := &settings{ctx: ctx, extra: map[string]any{}}
		relay, key, err = strip(s.secret("BUZZ_RELAY_URL")), s.privateKey(), s.err
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Buzz: requirement check could not read settings: %v", err))
		return false
	}
	return relay != "" && key != ""
}

// Whether the platform config resolves a relay URL and a private key
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	s := &settings{ctx: ctx, extra: extra}
	var relay string
	if gateway.ProfileScoped(ctx) {
		raw, ok := s.lookup("BUZZ_RELAY_URL", "relay_url")
		if !ok {
			raw = extra["relay_url"]
		}
		relay = textOf(raw)
	} else {
		relay = s.secret("BUZZ_RELAY_URL")
		if relay == "" {
			relay = textOf(extra["relay_url"])
		}
	}
	key := s.privateKey()
	if s.err != nil {
		return false, s.err
	}
	return relay != "" && key != "", nil
}

// Extra seeded from an env only setup so gateway status shows it, nil when unconfigured
func envEnablement(ctx context.Context) map[string]any {
	s := &settings{ctx: ctx, extra: map[string]any{}}
	relay := strip(s.secret("BUZZ_RELAY_URL"))
	key := s.privateKey()
	seedRead := func(name string) string {
		v, err := gateway.GetScopedSecret(ctx, name, "", false)
		s.fail(err)
		return strip(v)
	}
	channelsRaw, pollRaw, cliRaw := seedRead("BUZZ_CHANNELS"), seedRead("BUZZ_POLL_INTERVAL"), seedRead("BUZZ_CLI_PATH")
	if s.err != nil {
		slog.Debug(fmt.Sprintf("Buzz: env enablement could not read settings: %v", s.err))
		return nil
	}
	if relay == "" || key == "" {
		return nil
	}
	seed := map[string]any{"relay_url": relay}
	firstChannel := ""
	if channelsRaw != "" {
		var channels []any
		for _, c := range strings.Split(channelsRaw, ",") {
			if c = strip(c); c != "" {
				channels = append(channels, c)
			}
		}
		if channels == nil {
			channels = []any{}
		}
		seed["channels"] = channels
		if len(channels) > 0 {
			firstChannel = channels[0].(string)
		}
	}
	if pollRaw != "" {
		if v, err := strconv.ParseFloat(pollRaw, 64); err == nil {
			seed["poll_interval"] = v
		}
	}
	if cliRaw != "" {
		seed["cli_path"] = cliRaw
	}
	homeSeed, err := gateway.SeedExtraFromEnv(ctx, nil, "BUZZ_HOME_CHANNEL", firstChannel)
	if err != nil {
		slog.Debug(fmt.Sprintf("Buzz: env enablement could not read the home channel: %v", err))
		return nil
	}
	for k, v := range homeSeed {
		seed[k] = v
	}
	return seed
}

// One shot send through the buzz CLI without a live adapter, for cron delivery and icebath send
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID := textOf(kwargs["thread_id"])
	media, _ := kwargs["media_files"].([]gateway.MediaRef)
	extra := map[string]any{}
	if pconfig != nil && pconfig.Extra != nil {
		extra = pconfig.Extra
	}
	s := &settings{ctx: ctx, extra: extra}
	relay := s.relayURL()
	key := s.privateKey()
	tag, err := s.authTag()
	if err != nil {
		if _, isTag := err.(*authTagError); isTag {
			return gateway.SendError("Buzz standalone send: " + err.Error()), nil
		}
		return nil, err
	}
	cliPath := s.cliPath()
	target := strip(chatID)
	if target == "" {
		target = s.homeChannel()
	}
	mode := s.replyToMode(pconfig)
	if s.err != nil {
		return nil, s.err
	}
	if relay == "" || key == "" {
		return gateway.SendError("Buzz standalone send: BUZZ_RELAY_URL and BUZZ_PRIVATE_KEY must be configured"), nil
	}
	if cliPath == "" {
		return gateway.SendError("Buzz standalone send: buzz CLI binary not found"), nil
	}
	if target == "" {
		return gateway.SendError("Buzz standalone send: no target channel (set BUZZ_HOME_CHANNEL)"), nil
	}
	args := []string{"messages", "send", "--channel", target, "--content", "-"}
	if threadID != "" && mode != "off" {
		args = append(args, "--reply-to", threadID)
	}
	for _, m := range media {
		args = append(args, "--file", m.Path)
	}
	res, err := runBuzz(ctx, cliPath, args, relay, key, tag, &message)
	if err == nil && res.code != 0 {
		if escaped, ok := escapeUnresolvedPresentationMention(message, res.stderr); ok {
			slog.Info("Buzz: retrying standalone message after unresolved presentation-mention preflight")
			res, err = runBuzz(ctx, cliPath, args, relay, key, tag, &escaped)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("Buzz standalone send failed to launch CLI: " + boundedCLIMessage(err.Error(), "")), nil
	}
	if res.code != 0 {
		return gateway.SendError("Buzz standalone send failed: " + cliErrorMessage(res.stderr, res.code, "")), nil
	}
	eventID, receiptErr := parseSendReceipt(res.stdout)
	if receiptErr != "" {
		return gateway.SendError("Buzz standalone send failed: " + receiptErr), nil
	}
	result := map[string]any{"success": true, "message_id": eventID}
	if len(media) > 0 {
		result["media_delivered"] = true
	}
	return result, nil
}

// Buzz platform plugin compiled into the binary under the bundled platforms/buzz key
type buzzPlatformPlugin struct{}

func (buzzPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "buzz-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/buzz",
		Author: "heyer.app",
		Description: "Buzz gateway adapter for Icebath. Connects to a Buzz community relay (Block's open-source human+agent " +
			"collaboration platform built on Nostr) and relays messages between channels/DMs and the Icebath agent. Relay " +
			"operations run through the buzz CLI binary (JSON in/out); inbound arrives over a NIP-42 authenticated WebSocket " +
			"subscription with a CLI poll fallback.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "BUZZ_RELAY_URL", Description: "Base URL of the Buzz community relay (e.g. https://mycommunity.communities.buzz.xyz)"},
			{Name: "BUZZ_PRIVATE_KEY", Description: "Nostr private key for the agent's Buzz identity (nsec or hex) — the only Buzz secret", Secret: true}}}
}

// Registers the Buzz platform entry with its factory, gates, setup, YAML bridge, standalone sender and hint
func (buzzPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "buzz", Label: "Buzz",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"BUZZ_RELAY_URL", "BUZZ_PRIVATE_KEY"},
		InstallHint: "Requires the buzz CLI binary (https://github.com/block/buzz) on PATH or at BUZZ_CLI_PATH",
		Extra: map[string]any{
			"target_ref_syntax_fn":   parseSendTarget,
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"apply_yaml_config_fn":   applyYAMLConfig,
			"cron_deliver_env_var":   "BUZZ_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "BUZZ_ALLOWED_USERS",
			"allow_all_env":          "BUZZ_ALLOW_ALL_USERS",
			"emoji":                  "🐝",
			"pii_safe":               false,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
			"display_tier":           "medium",
		},
	})
	return err
}

var _ gateway.PlatformAdapter = (*Adapter)(nil)

func init() { plugins.RegisterBundled(buzzPlatformPlugin{}) }
