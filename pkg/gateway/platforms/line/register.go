package line

import (
	"context"
	"strconv"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env var, extra key and conversion of every LINE value seeded from an env only setup
var envSeedKeys = []gateway.EnvSeedRow{
	{Env: "LINE_PORT", Key: "port", Conv: func(raw string) (any, error) {
		n, err := coerceStrictInt(raw)
		if err != nil {
			return nil, err
		}
		return n, nil
	}},
	{Env: "LINE_HOST", Key: "host"},
	{Env: "LINE_PUBLIC_URL", Key: "public_url"},
}

// Python int of a string, the ValueError int raises for a bad literal
func coerceStrictInt(raw string) (int64, error) {
	n, err := pyint.Int64(raw)
	if pe, ok := err.(*pyint.Error); ok {
		return 0, &gateway.PyError{Kind: pe.Kind, Msg: pe.Msg}
	}
	return n, err
}

// Whether both channel credentials are set in the profile scoped env
func envCredentialsPresent(ctx context.Context) bool {
	for _, k := range []string{"LINE_CHANNEL_ACCESS_TOKEN", "LINE_CHANNEL_SECRET"} {
		v, err := gateway.GetScopedSecret(ctx, k, "", false)
		if err != nil || pystr.Strip(v) == "" {
			return false
		}
	}
	return true
}

// Plugin gate requiring both channel credentials, the webhook server being compiled in
func checkRequirements(ctx context.Context) bool { return envCredentialsPresent(ctx) }

// Whether the config resolves both channel credentials
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	token, secret, err := credentials(ctx, config)
	if err != nil {
		return false, err
	}
	return token != "" && secret != "", nil
}

// Seeds extra from an env only setup so status sees it, nil without credentials, a scope read failure raising
func envEnablement(ctx context.Context) map[string]any {
	if !envCredentialsPresent(ctx) {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedKeys, "LINE_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	return seed
}

// Out of process push delivery for detached cron jobs, media reported as not deliverable
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	token, _, err := credentials(ctx, pconfig)
	if err != nil {
		return gateway.SendError(err.Error()), nil
	}
	if token == "" || chatID == "" {
		return gateway.SendError("LINE standalone send: missing token or chat_id"), nil
	}
	messages := textMessages(message)
	if len(messages) == 0 {
		messages = []messagingMessage{textMessage("")}
	}
	if len(mediaFiles) > 0 {
		messages = append(messages, textMessage(i18n.T(ctx, "platform.line.standalone.attachments_not_deliverable", "",
			map[string]any{"count": strconv.Itoa(len(mediaFiles))})))
		if len(messages) > lineMaxMessagesPerCall {
			messages = messages[:lineMaxMessagesPerCall]
		}
	}
	if err := newLineClient(token).push(ctx, chatID, messages); err != nil {
		return gateway.SendError(err.Error()), nil
	}
	return map[string]any{"success": true, "message_id": nil}, nil
}

// Model facing description of LINE rendering and delivery limits
const platformHint = "You are chatting via LINE Messaging API. LINE does NOT render " +
	"Markdown — text bubbles show ** and # literally. Bare URLs are " +
	"auto-linked, but \\[label\\](url) syntax is not. Each text bubble " +
	"is capped at 5000 characters and at most 5 bubbles are sent per " +
	"reply, so keep responses concise. Image/audio/video sending " +
	"requires LINE_PUBLIC_URL configured to a publicly reachable HTTPS " +
	"host. Slow responses surface a 'Get answer' button the user taps " +
	"to fetch the reply via a fresh free token."

// LINE platform plugin compiled into the binary under the bundled platforms/line key
type linePlatformPlugin struct{}

func (linePlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "line-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/line",
		Author: "Icebath Agent contributors",
		Description: "LINE Messaging API gateway adapter for Icebath Agent. Runs a net/http webhook server that receives LINE " +
			"webhook events (with HMAC-SHA256 signature verification) and relays messages between LINE chats (1:1, groups, " +
			"rooms) and the Icebath agent. Outbound replies prefer the free reply token and fall back to the metered Push API " +
			"when the token has expired or is absent. Slow LLM responses surface a Template Buttons postback bubble so the " +
			"user can fetch the answer with a fresh reply token (free) once it's ready.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "LINE_CHANNEL_ACCESS_TOKEN", Description: "LINE channel long-lived access token (LINE Developers Console > Messaging API > Channel access token)",
				URL: "https://developers.line.biz/console/", Secret: true},
			{Name: "LINE_CHANNEL_SECRET", Description: "LINE channel secret (used for HMAC-SHA256 webhook signature verification)",
				URL: "https://developers.line.biz/console/", Secret: true}}}
}

// Adapter class the gateway migration code reads the profile prefix trait from
var lineAdapterClass = &gateway.AdapterClass{Name: "LineAdapter", Traits: lineAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewLineAdapter(ctx, config)
	}}

// Registers the LINE platform entry with its factory, gates, setup, standalone sender and hint
func (linePlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "line", Label: "LINE",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewLineAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"LINE_CHANNEL_ACCESS_TOKEN", "LINE_CHANNEL_SECRET"},
		Extra: map[string]any{
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"dashboard_description":  "Use Icebath from LINE via the LINE Messaging API webhook.",
			"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/line",
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "LINE_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "LINE_ALLOWED_USERS",
			"allow_all_env":          "LINE_ALLOW_ALL_USERS",
			"max_message_length":     lineSafeBubbleChars,
			"emoji":                  "💚",
			"pii_safe":               false,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(lineAdapterClass)
	plugins.RegisterBundled(linePlatformPlugin{})
}
