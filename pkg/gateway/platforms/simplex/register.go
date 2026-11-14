package simplex

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env var, extra key and conversion of every SimpleX value seeded from an env only setup
var envSeedKeys = []gateway.EnvSeedRow{
	{Env: "SIMPLEX_AUTO_ACCEPT", Key: "auto_accept", Conv: func(raw string) (any, error) {
		switch pystr.Lower(raw) {
		case "0", "false", "no":
			return false, nil
		}
		return true, nil
	}},
	{Env: "SIMPLEX_GROUP_ALLOWED", Key: "group_allowed"},
}

// Whether the scoped env names the daemon URL
func checkRequirements(ctx context.Context) bool {
	wsURL, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	return err == nil && wsURL != ""
}

// Whether the env or the config extra names the daemon URL
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	wsURL, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	if err != nil {
		return false, err
	}
	if wsURL != "" {
		return true, nil
	}
	if config == nil {
		return false, nil
	}
	return pyrepr.Truthy(config.Extra["ws_url"]), nil
}

// Whether SimpleX is configured through the env or the config extra
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	return validateConfig(ctx, config)
}

// Extra seeded from an env only setup, nil when the daemon URL is unset
func envEnablement(ctx context.Context) map[string]any {
	raw, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	if err != nil {
		panic(err)
	}
	wsURL := pystr.Strip(raw)
	if wsURL == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedKeys, "SIMPLEX_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	out := map[string]any{"ws_url": wsURL}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Sends the text body over a short lived daemon socket when no gateway runner lives in this process
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	scoped, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	if err != nil {
		return nil, err
	}
	var wsURL any = scoped
	if scoped == "" {
		wsURL = defaultWSURL
		if v, ok := extra["ws_url"]; ok {
			wsURL = v
		}
	}
	if !pyrepr.Truthy(wsURL) {
		return gateway.SendError("SimpleX standalone send: SIMPLEX_WS_URL is required"), nil
	}
	failed := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("SimpleX send failed: " + pyrepr.ExceptionMessage(err)), nil
	}
	command, err := sendCmd(chatID, []any{pyjson.ObjectOf("msgContent", pyjson.ObjectOf("type", "text", "text", message))})
	if err != nil {
		return failed(err)
	}
	frame, err := commandFrame(fmt.Sprintf("%ssnd-%d", corrPrefix, int64(nowSeconds()*1000)), command)
	if err != nil {
		return failed(err)
	}
	ws, err := pywebsockets.Connect(ctx, pyrepr.Str(wsURL), pywebsockets.ConnectOptions{OpenTimeout: 10 * time.Second,
		CloseTimeout: 5 * time.Second})
	if err != nil {
		return failed(err)
	}
	defer ws.Close()
	if err := ws.Send(frame); err != nil {
		return failed(err)
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]any{"success": true, "platform": "simplex", "chat_id": chatID}, nil
}

// Model facing description of SimpleX identity, formatting and attachments
const platformHint = "You are chatting via SimpleX Chat, a private decentralised " +
	"messenger. Contacts are identified by opaque internal IDs, " +
	"not phone numbers or usernames. SimpleX supports standard " +
	"markdown formatting. There is no typing indicator and no " +
	"hard message length limit, but keep responses conversational. " +
	"You can attach native images, voice notes, and arbitrary " +
	"files; the adapter handles MEDIA:<path> tags by sending them " +
	"as inline voice notes (audio extensions) or documents."

// SimpleX platform plugin compiled into the binary under the bundled platforms/simplex key
type simplexPlatformPlugin struct{}

func (simplexPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "simplex-platform", Version: "1.1.0", Kind: "platform", Source: "bundled", Key: "platforms/simplex",
		Author: "Mibayy, jooray",
		Description: "SimpleX Chat gateway adapter for Icebath Agent. Connects to a local simplex-chat daemon via WebSocket and " +
			"relays messages between SimpleX contacts/groups and the Icebath agent. SimpleX is decentralised and assigns no " +
			"persistent user IDs — every contact is an opaque internal ID generated at connection time, making it one of the " +
			"most private messengers available.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "SIMPLEX_WS_URL", Description: "WebSocket URL of the simplex-chat daemon (e.g. ws://127.0.0.1:5225)"}}}
}

// Adapter class the gateway reads the serial receive loop trait from
var simplexAdapterClass = &gateway.AdapterClass{Name: "SimplexAdapter", Traits: simplexAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewSimplexAdapter(ctx, config)
	}}

// Registers the SimpleX platform entry with its factory, gates, setup, env seeding, standalone sender and hint
func (simplexPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "simplex", Label: "SimpleX Chat",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewSimplexAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"SIMPLEX_WS_URL"},
		Extra: map[string]any{
			"dashboard_description":  "Talk to Icebath over SimpleX Chat via a local simplex-chat daemon.",
			"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/simplex",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           isConnected,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "SIMPLEX_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "SIMPLEX_ALLOWED_USERS",
			"allow_all_env":          "SIMPLEX_ALLOW_ALL_USERS",
			"max_message_length":     maxMessageLength,
			"emoji":                  "🔒",
			"pii_safe":               true,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(simplexAdapterClass)
	plugins.RegisterBundled(simplexPlatformPlugin{})
}
