package dingtalk

import (
	"cmp"
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Out of process delivery through the static robot webhook, the per session webhooks being out of reach of cron
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID := kwargs["chat_id"]
	message := kwargs["message"]
	var webhookURL any
	if pconfig != nil {
		webhookURL = pconfig.Extra["webhook_url"]
	}
	if !pyrepr.Truthy(webhookURL) {
		scoped, err := gateway.GetScopedSecret(ctx, "DINGTALK_WEBHOOK_URL", "", false)
		if err != nil {
			return nil, err
		}
		webhookURL = scoped
	}
	if !pyrepr.Truthy(webhookURL) {
		return gateway.SendError("DingTalk not configured. Set DINGTALK_WEBHOOK_URL env var or webhook_url in dingtalk platform extra config."), nil
	}
	target := pyrepr.Str(webhookURL)
	sendFailed := func(err error, text string) map[string]any {
		return gateway.SendError("DingTalk send failed: " + cmp.Or(gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(target)), text))
	}
	failed := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return sendFailed(err, pyrepr.ExceptionMessage(err)), nil
	}
	client := gateway.LimitedHTTPClient(gateway.DefaultHTTPXLimits(), false)
	defer client.CloseIdleConnections()
	resp, err := postJSON(ctx, client, target, pyjson.ObjectOf("msgtype", "text", "text", pyjson.ObjectOf("content", message)), 30*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return sendFailed(err, pyhttpx.ErrorText(err)), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := resp.StatusErrorText(resp.URL)
		return sendFailed(&gateway.PyError{Kind: "HTTPStatusError", Msg: text}, text), nil
	}
	parsed, err := resp.JSON()
	if err != nil {
		return failed(err)
	}
	data, ok := parsed.(*pyjson.Object)
	if !ok {
		return failed(attributeError(parsed, "get"))
	}
	if errcode := data.GetOr("errcode", int64(0)); !pyrepr.Equal(errcode, int64(0)) {
		return gateway.SendError("DingTalk API error: " + pyrepr.Str(data.GetOr("errmsg", "unknown"))), nil
	}
	return map[string]any{"success": true, "platform": "dingtalk", "chat_id": chatID}, nil
}

// Whether the config extra or the env carries both the client id and the client secret
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	clientID, clientSecret, err := credentials(ctx, extra)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(clientID) && pyrepr.Truthy(clientSecret), nil
}

// DingTalk platform plugin compiled into the binary under the bundled platforms/dingtalk key
type dingtalkPlatformPlugin struct{}

func (dingtalkPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "dingtalk-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/dingtalk",
		Author: "Icebath",
		Description: "DingTalk gateway adapter for Icebath Agent. Connects to DingTalk via the dingtalk-stream SDK (Stream Mode) " +
			"and relays messages between DingTalk chats and the Icebath agent. Supports text, images, audio, video, rich text, " +
			"files, group @mention gating, free-response chats, and per-user allowlists.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "DINGTALK_CLIENT_ID", Description: "DingTalk app key (Client ID)", URL: "https://open-dev.dingtalk.com"},
			{Name: "DINGTALK_CLIENT_SECRET", Description: "DingTalk app secret (Client Secret)", URL: "https://open-dev.dingtalk.com",
				Secret: true}}}
}

// Adapter class of the DingTalk Stream Mode adapter
var dingtalkAdapterClass = &gateway.AdapterClass{Name: "DingTalkAdapter", Traits: dingtalkAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewDingTalkAdapter(ctx, config)
	}}

// Registers the DingTalk platform entry with its factory, gates, setup, YAML bridge, env bridge and standalone sender
func (dingtalkPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"standalone_send":        &gateway.StandaloneSendPolicy{TextDirect: true},
		"ensure_deps_fn":         EnsureDingTalkDeps,
		"is_connected":           isConnected,
		"scoped_validate_config": isConnected,
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "DINGTALK_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"emoji":                  "🐳",
		"allow_update_command":   true,
		"display_tier":           "low",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "dingtalk", Label: "DingTalk",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewDingTalkAdapter(fctx, cfg)
		}),
		Check:       DingTalkDepsPresent,
		RequiredEnv: []string{"DINGTALK_CLIENT_ID", "DINGTALK_CLIENT_SECRET"},
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(dingtalkAdapterClass)
	plugins.RegisterBundled(dingtalkPlatformPlugin{})
}
