package buzz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Invalid owner attestation configuration, reported as a fatal config error
type authTagError struct{ msg string }

func (e *authTagError) Error() string { return e.msg }

// Text of a setting value, empty for nil
func settingText(v any) string {
	if v == nil {
		return ""
	}
	return anyutil.String(v)
}

// Reply mode first, all or off from buzz.reply_to_mode, buzz.reply_in_thread off meaning off
func replyToMode(ctx context.Context) string {
	sctx := gateway.SettingsContext(ctx)
	if !settings.BuzzReplyInThread.Get(sctx) {
		return "off"
	}
	return settings.BuzzReplyToMode.Get(sctx)
}

// Relay URL from buzz.relay_url, stripped
func configuredRelay(ctx context.Context) string {
	return strings.TrimSpace(settings.BuzzRelayURL.Get(gateway.SettingsContext(ctx)))
}

// Channels the adapter watches from buzz.channels, blanks dropped
func configuredChannels(ctx context.Context) []string {
	var out []string
	for _, c := range settings.BuzzChannels.Strings(gateway.SettingsContext(ctx)) {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Home channel from the scoped BUZZ_HOME_CHANNEL, else the home_channel of the connection block, stripped
func configuredHomeChannel(ctx context.Context, extra map[string]any) (string, error) {
	var text string
	if gateway.ProfileScoped(ctx) {
		val, found, err := gateway.LookupScopedSecret(ctx, "BUZZ_HOME_CHANNEL", true)
		if err != nil {
			return "", err
		}
		if found {
			text = val
		}
	} else {
		text = os.Getenv("BUZZ_HOME_CHANNEL")
	}
	if strings.TrimSpace(text) == "" {
		text = settingText(extra["home_channel"])
	}
	return strings.TrimSpace(text), nil
}

// Resolved buzz binary from buzz.cli_path, empty when none exists
func configuredCLIPath(ctx context.Context) string {
	return resolveCLIPath(strings.TrimSpace(settings.BuzzCliPath.Get(gateway.SettingsContext(ctx))))
}

// Validated NIP-OA owner attestation tag as compact JSON, empty when none is configured
func resolveAuthTag(ctx context.Context, extra map[string]any) (string, error) {
	rawText, err := getScopedSecret(ctx, "BUZZ_AUTH_TAG", "")
	if err != nil {
		return "", err
	}
	var raw any = strings.TrimSpace(rawText)
	if rawText = strings.TrimSpace(rawText); rawText == "" {
		key, err := getScopedSecret(ctx, "BUZZ_PRIVATE_KEY", "")
		if err != nil {
			return "", err
		}
		file, err := configuredCredentialsFile(ctx, extra)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(key) != "" && file == "" {
			return "", nil
		}
		data, err := resolveCredentialsData(ctx, extra)
		if err != nil {
			return "", err
		}
		tag, ok := data.Get("auth_tag")
		if !ok {
			return "", nil
		}
		raw = tag
	}
	parsed, err := ParseAuthTag(raw, "Buzz auth tag")
	if err != nil {
		return "", &authTagError{err.Error()}
	}
	items := make([]any, len(parsed))
	for i, s := range parsed {
		items[i] = s
	}
	return jsonutil.Encode(items)
}

// Extra of the buzz section in the scoped profile's config, empty when unscoped or unreadable
func profileBuzzExtra(ctx context.Context) map[string]any {
	if !gateway.ProfileScoped(ctx) {
		return map[string]any{}
	}
	cfg, err := config.ReadUserConfigRaw(ctx, filepath.Join(home.Dir(ctx), "config.yaml"))
	if err != nil || cfg == nil {
		return map[string]any{}
	}
	section, _ := gateway.PlatformSection(cfg, "buzz")
	buzz, ok := section.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	extra := any(buzz)
	if v, ok := buzz["extra"]; ok {
		extra = v
	}
	if m, ok := extra.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Whether a relay URL and a resolvable key are configured for the current profile
func checkRequirements(ctx context.Context) bool {
	if configuredRelay(ctx) == "" {
		return false
	}
	var extra map[string]any
	if gateway.ProfileScoped(ctx) {
		extra = profileBuzzExtra(ctx)
	}
	key, err := resolvePrivateKey(ctx, extra)
	return err == nil && key != ""
}

// Whether the profile has a relay URL and the platform config a resolvable key
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if configuredRelay(ctx) == "" {
		return false, nil
	}
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	key, err := resolvePrivateKey(ctx, extra)
	return key != "", err
}

// Extra seeded for the gateway status so setups without a config block show there, nil when unconfigured
func envEnablement(ctx context.Context) map[string]any {
	relay := configuredRelay(ctx)
	if relay == "" {
		return nil
	}
	if key, err := resolvePrivateKey(ctx, nil); err != nil {
		panic(err)
	} else if key == "" {
		return nil
	}
	homeDefault := ""
	if chans := configuredChannels(ctx); len(chans) > 0 {
		homeDefault = chans[0]
	}
	homeSeed, err := gateway.SeedExtraFromEnv(ctx, nil, "BUZZ_HOME_CHANNEL", homeDefault)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"relay_url": relay}
	for k, v := range homeSeed {
		out[k] = v
	}
	return out
}

// One shot send without a live adapter for out of process cron delivery
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID := ""
	if v := kwargs["thread_id"]; v != nil {
		threadID = anyutil.String(v)
	}
	extra := map[string]any{}
	if pconfig != nil && pconfig.Extra != nil {
		extra = pconfig.Extra
	}
	relay := configuredRelay(ctx)
	privateKey, err := resolvePrivateKey(ctx, extra)
	if err != nil {
		return nil, err
	}
	authTag, err := resolveAuthTag(ctx, extra)
	var invalid *authTagError
	if errors.As(err, &invalid) {
		return gateway.SendError("Buzz standalone send: " + invalid.Error()), nil
	}
	if err != nil {
		return nil, err
	}
	cliPath := configuredCLIPath(ctx)
	if relay == "" || privateKey == "" {
		return gateway.SendError("Buzz standalone send: BUZZ_RELAY_URL and BUZZ_PRIVATE_KEY must be configured"), nil
	}
	if cliPath == "" {
		return gateway.SendError("Buzz standalone send: buzz CLI binary not found"), nil
	}
	target := strings.TrimSpace(chatID)
	if target == "" {
		if target, err = configuredHomeChannel(ctx, extra); err != nil {
			return nil, err
		}
	}
	if target == "" {
		return gateway.SendError("Buzz standalone send: no target channel (set BUZZ_HOME_CHANNEL)"), nil
	}
	args := []string{"messages", "send", "--channel", target, "--content", "-"}
	if threadID != "" {
		if replyToMode(ctx) != "off" {
			args = append(args, "--reply-to", threadID)
		}
	}
	var mediaFiles []string
	switch v := kwargs["media_files"].(type) {
	case []gateway.MediaRef:
		for _, m := range v {
			mediaFiles = append(mediaFiles, m.Path)
		}
	case []any:
		for _, media := range v {
			if pair, ok := media.([]any); ok && len(pair) > 0 {
				media = pair[0]
			}
			mediaFiles = append(mediaFiles, anyutil.String(media))
		}
	}
	for _, media := range mediaFiles {
		args = append(args, "--file", media)
	}
	res, err := execBuzz(ctx, cliPath, args, relay, privateKey, authTag, &message)
	if err == nil && res.code != 0 {
		if escaped, ok := escapeUnresolvedPresentationMention(message, res.stderr); ok {
			pkgLog().Info("Buzz: retrying standalone message after unresolved presentation-mention preflight")
			res, err = execBuzz(ctx, cliPath, args, relay, privateKey, authTag, &escaped)
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
	if len(mediaFiles) > 0 {
		result["media_delivered"] = true
	}
	return result, nil
}

// Buzz channel UUIDs are explicit native send targets
const sendUUIDPattern = `(?i)^\s*[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\s*$`

// Model facing description of Buzz conversations
const platformHint = "You are collaborating in a Buzz workspace (Block's Nostr-based " +
	"human+agent platform). Markdown IS supported. Users address you " +
	"by @-mentioning your name or npub in channels; direct messages " +
	"reach you without a mention. Keep responses conversational."

// Buzz platform plugin compiled into the binary under the bundled platforms/buzz key
type buzzPlatformPlugin struct{}

func (buzzPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "buzz-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/buzz",
		Description: "Buzz gateway adapter for Icebath Agent. Connects to a Buzz community relay (Block's open-source " +
			"human+agent collaboration platform built on Nostr) and relays messages between channels/DMs and the Icebath " +
			"agent. Relay operations shell out to the buzz CLI binary (JSON in/out); verified inbound media uses the " +
			"Icebath HTTP client.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "BUZZ_RELAY_URL", Description: "Base URL of the Buzz community relay (e.g. https://mycommunity.communities.buzz.xyz)"},
			{Name: "BUZZ_PRIVATE_KEY", Description: "Nostr private key for the agent's Buzz identity (nsec or hex) — the only Buzz secret", Secret: true},
		}}
}

// Adapter class the gateway reads the serial receive loop trait from
var buzzAdapterClass = &gateway.AdapterClass{Name: "BuzzAdapter", Traits: buzzAdapterTraits(),
	New: func(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewBuzzAdapter(ctx, cfg)
	}}

// Registers the Buzz platform entry with its factory, gates, env seeding, YAML bridge, standalone sender and hint
func (buzzPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "buzz", Label: "Buzz",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewBuzzAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"BUZZ_RELAY_URL", "BUZZ_PRIVATE_KEY"},
		InstallHint: "Requires the buzz CLI binary (https://github.com/block/buzz) on PATH or at BUZZ_CLI_PATH",
		Extra: map[string]any{
			"target_ref_syntax_fn":   gateway.RegexStrippedParser(sendUUIDPattern),
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
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
	if err != nil {
		return fmt.Errorf("buzz: %w", err)
	}
	return nil
}

// Registers the adapter module and the bundled plugin
func init() {
	gateway.RegisterAdapterModule(buzzAdapterClass)
	plugins.RegisterBundled(buzzPlatformPlugin{})
}
