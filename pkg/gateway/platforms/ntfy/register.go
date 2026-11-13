package ntfy

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env var, extra key and conversion of every ntfy value seeded from an env only setup
var envSeedRows = []gateway.EnvSeedRow{
	{Env: "NTFY_SERVER_URL", Key: "server", Conv: func(raw string) (any, error) { return strings.TrimRight(raw, "/"), nil }},
	{Env: "NTFY_PUBLISH_TOPIC", Key: "publish_topic"},
	{Env: "NTFY_TOKEN", Key: "token"},
	{Env: "NTFY_MARKDOWN", Key: "markdown", Conv: func(raw string) (any, error) { return markdownTruthy[strings.ToLower(raw)], nil }},
}

// Scoped subscribe topic, empty when unset
func envTopic(ctx context.Context) (string, error) {
	topic, err := gateway.GetScopedSecret(ctx, "NTFY_TOPIC", "", false)
	return strings.TrimSpace(topic), err
}

// Plugin gate requiring the subscribe topic in the scoped env
func checkRequirements(ctx context.Context) bool {
	topic, err := envTopic(ctx)
	return err == nil && topic != ""
}

// Whether a subscribe topic is configured in the scoped env or extra
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	topic, err := setting(ctx, extra, "topic", "NTFY_TOPIC", "")
	return topic != "", err
}

// Seeds extra from an env only setup so status sees it, nil without a topic, a scope read failure raising
func envEnablement(ctx context.Context) map[string]any {
	topic, err := envTopic(ctx)
	if err != nil {
		panic(err)
	}
	if topic == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedRows, "NTFY_HOME_CHANNEL", topic)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"topic": topic, "server": defaultServer}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Local paths of the standalone media argument
func mediaPaths(raw any) []string {
	var out []string
	switch list := raw.(type) {
	case []gateway.MediaRef:
		for _, m := range list {
			out = append(out, m.Path)
		}
	case []string:
		out = append(out, list...)
	case []any:
		for _, item := range list {
			switch x := item.(type) {
			case string:
				out = append(out, x)
			case gateway.MediaRef:
				out = append(out, x.Path)
			case map[string]any:
				out = append(out, textOf(x["path"]))
			}
		}
	}
	return out
}

// Standalone target topic, the chat first, then the publish topic, then the subscribe topic
func standaloneTopic(ctx context.Context, chatID string, extra map[string]any) (string, error) {
	for _, step := range []struct{ key, env string }{{"", ""}, {"publish_topic", "NTFY_PUBLISH_TOPIC"}, {"topic", "NTFY_TOPIC"}} {
		value := chatID
		if step.key != "" {
			value = textOf(extra[step.key])
		}
		if value = strings.TrimSpace(value); value != "" {
			return value, nil
		}
		if step.env == "" {
			continue
		}
		env, err := gateway.GetScopedSecret(ctx, step.env, "", false)
		if err != nil {
			return "", err
		}
		if env = strings.TrimSpace(env); env != "" {
			return env, nil
		}
	}
	return "", nil
}

// Out of process publish to the chat topic or the configured publish topic, media uploaded as attachments
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	server, err := serverURL(ctx, extra)
	if err != nil {
		return gateway.SendError("ntfy standalone send failed: " + err.Error()), nil
	}
	topic, err := standaloneTopic(ctx, chatID, extra)
	if err != nil {
		return gateway.SendError("ntfy standalone send failed: " + err.Error()), nil
	}
	if topic == "" {
		return gateway.SendError("ntfy standalone send: NTFY_TOPIC not configured"), nil
	}
	token, err := setting(ctx, extra, "token", "NTFY_TOKEN", "")
	if err != nil {
		return gateway.SendError("ntfy standalone send failed: " + err.Error()), nil
	}
	markdown, err := markdownEnabled(ctx, extra)
	if err != nil {
		return gateway.SendError("ntfy standalone send failed: " + err.Error()), nil
	}
	client := &http.Client{Transport: newTransport(ctx)}
	defer client.CloseIdleConnections()
	run := func(p publishRequest) (string, map[string]any, error) {
		id, failure, err := publish(ctx, client, server, token, p)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			return "", gateway.SendError("ntfy standalone send failed: " + err.Error()), nil
		}
		if failure != "" {
			return "", gateway.SendError("ntfy " + failure), nil
		}
		return id, nil, nil
	}
	media := mediaPaths(kwargs["media_files"])
	lastID := ""
	if strings.TrimSpace(message) != "" || len(media) == 0 {
		for _, chunk := range bodyChunks(message) {
			id, failed, err := run(publishRequest{topic: topic, body: []byte(chunk), markdown: markdown})
			if err != nil || failed != nil {
				return failed, err
			}
			lastID = id
		}
	}
	uploaded, skipped := 0, 0
	for _, path := range media {
		data, err := os.ReadFile(path)
		if path == "" || err != nil {
			skipped++
			continue
		}
		id, failed, err := run(filePublish(topic, path, "", "", markdown, data))
		if err != nil || failed != nil {
			return failed, err
		}
		lastID = id
		uploaded++
	}
	result := map[string]any{"success": true, "platform": "ntfy", "chat_id": topic, "message_id": lastID}
	if uploaded > 0 && skipped == 0 {
		result["media_delivered"] = true
	}
	return result, nil
}

// Model facing description of ntfy rendering, limits and attachments
const platformHint = "You are communicating via ntfy push notifications. " +
	"Use plain text by default — ntfy supports optional markdown " +
	"(set markdown: true in config or NTFY_MARKDOWN=true). " +
	"Keep responses concise; ntfy is a push notification service " +
	"with a 4096-byte per-message limit. You can send files as attachments: " +
	"include MEDIA:/absolute/path/to/file in your response."

// ntfy platform plugin compiled into the binary under the bundled platforms/ntfy key
type platformPlugin struct{}

func (platformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "ntfy-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/ntfy",
		Author: "heyer.app",
		Description: "ntfy push notification gateway adapter for Icebath. Subscribes to a topic on ntfy.sh or any self-hosted ntfy " +
			"server over the JSON stream and publishes replies and attachments over HTTP. ntfy has no user identity, so each " +
			"topic is one trusted channel. Use a private topic and an access token for any real trust boundary.\n",
		RequiresEnv: []plugins.EnvSpec{{Name: "NTFY_TOPIC", Description: "Topic name to subscribe to (e.g. icebath-in)"}}}
}

// Registers the ntfy platform with its factory, gates, env seeding, standalone sender and hint
func (platformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "ntfy", Label: "ntfy",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"NTFY_TOPIC"},
		Extra: map[string]any{
			"target_ref_syntax_fn":   gateway.ParseNonempty,
			"dashboard_description":  "Chat with Icebath over ntfy push topics (ntfy.sh or self-hosted).",
			"docs_url":               "https://docs.ntfy.sh/",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "NTFY_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "NTFY_ALLOWED_USERS",
			"allow_all_env":          "NTFY_ALLOW_ALL_USERS",
			"max_message_length":     maxBodyBytes,
			"emoji":                  "🔔",
			"pii_safe":               true,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	plugins.RegisterBundled(platformPlugin{})
}
