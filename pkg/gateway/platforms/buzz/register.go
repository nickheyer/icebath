package buzz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Invalid owner attestation configuration, reported as a fatal config error
type authTagError struct{ msg string }

func (e *authTagError) Error() string { return e.msg }

// Raw non secret setting, the profile's own secret scope then the extra inside a secondary profile, the process env otherwise
func scopedPlatformSetting(ctx context.Context, envName string, extra map[string]any, key string) (any, error) {
	if gateway.ProfileScoped(ctx) {
		val, found, err := gateway.LookupScopedSecret(ctx, envName, true)
		if err != nil {
			return nil, err
		}
		if found {
			return val, nil
		}
		return extra[key], nil
	}
	if val, ok := os.LookupEnv(envName); ok {
		return val, nil
	}
	return nil, nil
}

// Scoped setting with an explicit fallback to the extra value, then the default
func settingOr(ctx context.Context, envName string, extra map[string]any, key string, def any) (any, error) {
	raw, err := scopedPlatformSetting(ctx, envName, extra, key)
	if err != nil || raw != nil {
		return raw, err
	}
	if v, ok := extra[key]; ok {
		return v, nil
	}
	return def, nil
}

// Text of a setting value, empty for nil
func settingText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Reply mode first, all or off, env over config and reply_in_thread false meaning off
func replyToMode(ctx context.Context, cfg *gateway.PlatformConfig, extra map[string]any) (string, error) {
	var modeEnv string
	if gateway.ProfileScoped(ctx) {
		v, err := getScopedSecret(ctx, "BUZZ_REPLY_TO_MODE", "")
		if err != nil {
			return "", err
		}
		modeEnv = v
	} else {
		modeEnv = os.Getenv("BUZZ_REPLY_TO_MODE")
	}
	mode := modeEnv
	if mode == "" && cfg != nil && pyrepr.Truthy(cfg.ReplyToMode) {
		mode = pyrepr.Str(cfg.ReplyToMode)
	}
	if mode == "" {
		mode = "first"
	}
	mode = strings.ToLower(pystr.Strip(mode))
	rit, err := settingOr(ctx, "BUZZ_REPLY_IN_THREAD", extra, "reply_in_thread", nil)
	if err != nil {
		return "", err
	}
	if rit != nil && falsy(rit) {
		return "off", nil
	}
	return mode, nil
}

// Relay URL from the scoped setting or the extra, stripped
func configuredRelay(ctx context.Context, extra map[string]any) (string, error) {
	raw, err := scopedPlatformSetting(ctx, "BUZZ_RELAY_URL", extra, "relay_url")
	if err != nil {
		return "", err
	}
	text := settingText(raw)
	if text == "" {
		text = settingText(extra["relay_url"])
	}
	return pystr.Strip(text), nil
}

// Home channel from the scoped setting or the extra, stripped
func configuredHomeChannel(ctx context.Context, extra map[string]any) (string, error) {
	raw, err := scopedPlatformSetting(ctx, "BUZZ_HOME_CHANNEL", extra, "home_channel")
	if err != nil {
		return "", err
	}
	text := settingText(raw)
	if text == "" {
		text = settingText(extra["home_channel"])
	}
	return pystr.Strip(text), nil
}

// Resolved buzz binary from the scoped setting or the extra, empty when none exists
func configuredCLIPath(ctx context.Context, extra map[string]any) (string, error) {
	raw, err := scopedPlatformSetting(ctx, "BUZZ_CLI_PATH", extra, "cli_path")
	if err != nil {
		return "", err
	}
	configured := pystr.Strip(settingText(raw))
	if configured == "" {
		configured = settingText(extra["cli_path"])
	}
	return resolveCLIPath(configured), nil
}

// Validated NIP-OA owner attestation tag as compact JSON, empty when none is configured
func resolveAuthTag(ctx context.Context, extra map[string]any) (string, error) {
	rawText, err := getScopedSecret(ctx, "BUZZ_AUTH_TAG", "")
	if err != nil {
		return "", err
	}
	var raw any = pystr.Strip(rawText)
	if rawText = pystr.Strip(rawText); rawText == "" {
		key, err := getScopedSecret(ctx, "BUZZ_PRIVATE_KEY", "")
		if err != nil {
			return "", err
		}
		file, err := configuredCredentialsFile(ctx, extra)
		if err != nil {
			return "", err
		}
		if pystr.Strip(key) != "" && file == "" {
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
	return pyjson.Dumps(items, pyjson.Separators(",", ":"))
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
	var gatewayPlatforms any
	if gw, ok := cfg["gateway"].(map[string]any); ok {
		gatewayPlatforms = gw["platforms"]
	}
	section, _ := gateway.PlatformSection(cfg, "buzz", gatewayPlatforms)
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
	if gateway.ProfileScoped(ctx) {
		extra := profileBuzzExtra(ctx)
		relay, err := configuredRelay(ctx, extra)
		if err != nil || relay == "" {
			return false
		}
		key, err := resolvePrivateKey(ctx, extra)
		return err == nil && key != ""
	}
	relay, err := getScopedSecret(ctx, "BUZZ_RELAY_URL", "")
	if err != nil || pystr.Strip(relay) == "" {
		return false
	}
	key, err := resolvePrivateKey(ctx, nil)
	return err == nil && key != ""
}

// Whether the platform config has a relay URL and a resolvable key
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	var relay string
	if gateway.ProfileScoped(ctx) {
		raw, err := scopedPlatformSetting(ctx, "BUZZ_RELAY_URL", extra, "relay_url")
		if err != nil {
			return false, err
		}
		if raw == nil {
			raw = extra["relay_url"]
		}
		relay = settingText(raw)
	} else {
		v, err := getScopedSecret(ctx, "BUZZ_RELAY_URL", "")
		if err != nil {
			return false, err
		}
		relay = v
		if relay == "" {
			relay = settingText(extra["relay_url"])
		}
	}
	if relay == "" {
		return false, nil
	}
	key, err := resolvePrivateKey(ctx, extra)
	return key != "", err
}

// Extra key, env var and kind of every setting the config.yaml bridge carries into the env
var yamlBridge = []gateway.YAMLBridgeRow{
	{Key: "relay_url", Env: "BUZZ_RELAY_URL", Kind: "str"}, {Key: "cli_path", Env: "BUZZ_CLI_PATH", Kind: "str"},
	{Key: "home_channel", Env: "BUZZ_HOME_CHANNEL", Kind: "str"}, {Key: "transport", Env: "BUZZ_TRANSPORT", Kind: "str"},
	{Key: "poll_interval", Env: "BUZZ_POLL_INTERVAL", Kind: "str"},
	{Key: "channels", Env: "BUZZ_CHANNELS", Kind: "csv"}, {Key: "allowed_users", Env: "BUZZ_ALLOWED_USERS", Kind: "csv"},
	{Key: "reaction_only_users", Env: "BUZZ_REACTION_ONLY_USERS", Kind: "csv"}, {Key: "allow_all_users", Env: "BUZZ_ALLOW_ALL_USERS", Kind: "lower"},
	{Key: "require_mention", Env: "BUZZ_REQUIRE_MENTION", Kind: "lower"}, {Key: "reply_in_thread", Env: "BUZZ_REPLY_IN_THREAD", Kind: "lower"},
	{Key: "reply_to_mode", Env: "BUZZ_REPLY_TO_MODE", Kind: "lower"},
}

// Bridges buzz.extra into the BUZZ env and the returned extra, the private key never read from config.yaml
func applyYAMLConfig(ctx context.Context, yamlCfg, buzzCfg map[string]any) (map[string]any, error) {
	var extra any = buzzCfg
	if v, ok := buzzCfg["extra"]; ok {
		extra = v
	}
	if !pyrepr.Truthy(extra) {
		extra = map[string]any{}
	}
	m, ok := extra.(map[string]any)
	if !ok {
		return nil, nil
	}
	return gateway.ApplyYAMLBridge(ctx, m, yamlBridge)
}

// Extra seeded from the profile env so env only setups show in gateway status, nil when unconfigured
func envEnablement(ctx context.Context) map[string]any {
	relayRaw, err := getScopedSecret(ctx, "BUZZ_RELAY_URL", "")
	if err != nil {
		panic(err)
	}
	relay := pystr.Strip(relayRaw)
	if relay == "" {
		return nil
	}
	if key, err := resolvePrivateKey(ctx, nil); err != nil {
		panic(err)
	} else if key == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, []gateway.EnvSeedRow{
		{Env: "BUZZ_CHANNELS", Key: "channels", Conv: func(v string) (any, error) {
			out := []any{}
			for _, c := range strings.Split(v, ",") {
				if c = pystr.Strip(c); c != "" {
					out = append(out, c)
				}
			}
			return out, nil
		}},
		{Env: "BUZZ_POLL_INTERVAL", Key: "poll_interval", Conv: func(v string) (any, error) {
			return strconv.ParseFloat(pystr.Strip(v), 64)
		}},
		{Env: "BUZZ_CLI_PATH", Key: "cli_path"},
	}, "", "")
	if err != nil {
		panic(err)
	}
	homeDefault := ""
	if chans, ok := seed["channels"].([]any); ok && len(chans) > 0 {
		homeDefault = pyrepr.Str(chans[0])
	}
	homeSeed, err := gateway.SeedExtraFromEnv(ctx, nil, "BUZZ_HOME_CHANNEL", homeDefault)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"relay_url": relay}
	for k, v := range seed {
		out[k] = v
	}
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
		threadID = pyrepr.Str(v)
	}
	extra := map[string]any{}
	if pconfig != nil && pconfig.Extra != nil {
		extra = pconfig.Extra
	}
	relay, err := configuredRelay(ctx, extra)
	if err != nil {
		return nil, err
	}
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
	cliPath, err := configuredCLIPath(ctx, extra)
	if err != nil {
		return nil, err
	}
	if relay == "" || privateKey == "" {
		return gateway.SendError("Buzz standalone send: BUZZ_RELAY_URL and BUZZ_PRIVATE_KEY must be configured"), nil
	}
	if cliPath == "" {
		return gateway.SendError("Buzz standalone send: buzz CLI binary not found"), nil
	}
	target := pystr.Strip(chatID)
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
		mode, err := replyToMode(ctx, pconfig, extra)
		if err != nil {
			return nil, err
		}
		if mode != "off" {
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
			mediaFiles = append(mediaFiles, pyrepr.Str(media))
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
	if err != nil {
		return fmt.Errorf("buzz: %w", err)
	}
	return nil
}

func init() {
	gateway.RegisterAdapterModule(buzzAdapterClass)
	plugins.RegisterBundled(buzzPlatformPlugin{})
}
