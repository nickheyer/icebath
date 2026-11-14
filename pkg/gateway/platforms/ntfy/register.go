package ntfy

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env var, extra key and conversion of every ntfy value seeded from an env only setup
var envSeedKeys = []gateway.EnvSeedRow{
	{Env: "NTFY_SERVER_URL", Key: "server", Conv: func(raw string) (any, error) { return pystr.RstripChars(raw, "/"), nil }},
	{Env: "NTFY_PUBLISH_TOPIC", Key: "publish_topic"},
	{Env: "NTFY_TOKEN", Key: "token"},
	{Env: "NTFY_MARKDOWN", Key: "markdown", Conv: func(raw string) (any, error) { return markdownTruthy[pystr.Lower(raw)], nil }},
}

// Extra seeded from an env only setup with the home channel defaulting to the topic, nil without a topic
func envEnablement(ctx context.Context) map[string]any {
	raw, err := gateway.GetScopedSecret(ctx, "NTFY_TOPIC", "", false)
	if err != nil {
		panic(err)
	}
	topic := pystr.Strip(raw)
	if topic == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedKeys, "NTFY_HOME_CHANNEL", topic)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"topic": topic, "server": defaultServer}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Out of process publish for cron and the send message tool when no gateway adapter is live
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	extra := configExtra(pconfig)
	server, err := serverURL(ctx, extra)
	if err != nil {
		return nil, err
	}
	scopedStripped := func(name string) (string, error) {
		val, err := gateway.GetScopedSecret(ctx, name, "", false)
		return pystr.Strip(val), err
	}
	var publishTopic any = chatID
	if !pyrepr.Truthy(publishTopic) {
		publishTopic = extra["publish_topic"]
	}
	if !pyrepr.Truthy(publishTopic) {
		if publishTopic, err = scopedStripped("NTFY_PUBLISH_TOPIC"); err != nil {
			return nil, err
		}
	}
	if !pyrepr.Truthy(publishTopic) {
		publishTopic = extra["topic"]
	}
	if !pyrepr.Truthy(publishTopic) {
		if publishTopic, err = scopedStripped("NTFY_TOPIC"); err != nil {
			return nil, err
		}
	}
	if !pyrepr.Truthy(publishTopic) {
		return gateway.SendError("ntfy standalone send: NTFY_TOPIC not configured"), nil
	}
	token, err := gateway.ExtraOrSecret(ctx, extra, "token", "NTFY_TOKEN", "", true)
	if err != nil {
		return nil, err
	}
	markdownEnv, err := scopedStripped("NTFY_MARKDOWN")
	if err != nil {
		return nil, err
	}
	markdown := pyrepr.Truthy(extra["markdown"]) || markdownTruthy[pystr.Lower(markdownEnv)]
	headers, err := publishHeaders(token, markdown, false)
	if err != nil {
		return nil, err
	}
	body := truncateBody(message, "ntfy standalone")
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := publish(ctx, client, server+"/"+pyrepr.Str(publishTopic), body, headers)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("ntfy standalone send failed: " + pyhttpx.ErrorText(err)), nil
	}
	if resp.StatusCode >= 300 {
		return gateway.SendError(fmt.Sprintf("ntfy HTTP %d: %s", resp.StatusCode, pystr.Slice(resp.Text(), 0, 200))), nil
	}
	return map[string]any{"success": true, "platform": "ntfy", "chat_id": publishTopic, "message_id": responseMessageID(resp)}, nil
}

// Model facing description of ntfy formatting and the per message limit
const platformHint = "You are communicating via ntfy push notifications. " +
	"Use plain text by default — ntfy supports optional markdown " +
	"(set markdown: true in config or NTFY_MARKDOWN=true). " +
	"Keep responses concise; ntfy is a push notification service " +
	"with a 4096-character per-message limit."

// ntfy platform plugin compiled into the binary under the bundled platforms/ntfy key
type ntfyPlatformPlugin struct{}

func (ntfyPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "ntfy-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/ntfy",
		Author: "sprmn24",
		Description: "ntfy push-notification gateway adapter for Icebath Agent. Subscribes to a topic on ntfy.sh or any " +
			"self-hosted ntfy server via HTTP streaming, and publishes replies via HTTP POST. Lightweight — no external SDK, " +
			"only net/http (already an Icebath dependency).\n" +
			"ntfy has no native user-identity primitive; the adapter treats each topic as a single trusted channel and never " +
			"derives user identity from publisher-controlled fields. Use a private topic + read token for any real trust " +
			"boundary.\n",
		RequiresEnv: []plugins.EnvSpec{{Name: "NTFY_TOPIC", Description: "Topic name to subscribe to (e.g. icebath-in)"}}}
}

// Adapter class the gateway reads the serial receive loop trait from
var ntfyAdapterClass = &gateway.AdapterClass{Name: "NtfyAdapter", Traits: ntfyAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewNtfyAdapter(ctx, config)
	}}

// Registers the ntfy platform entry with its factory, gates, env seeding, standalone sender and hint
func (ntfyPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "ntfy", Label: "ntfy",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewNtfyAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"NTFY_TOPIC"},
		Extra: map[string]any{
			"target_ref_syntax_fn":   gateway.ParseNonempty,
			"dashboard_description":  "Chat with Icebath over ntfy push topics (ntfy.sh or self-hosted).",
			"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/ntfy",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           isConnected,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "NTFY_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "NTFY_ALLOWED_USERS",
			"allow_all_env":          "NTFY_ALLOW_ALL_USERS",
			"max_message_length":     maxMessageLength,
			"emoji":                  "🔔",
			"pii_safe":               true,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(ntfyAdapterClass)
	plugins.RegisterBundled(ntfyPlatformPlugin{})
}
