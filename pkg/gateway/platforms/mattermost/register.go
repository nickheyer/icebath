package mattermost

import (
	"strings"

	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Paths of the media_files argument, a dict item read through its path key
func standaloneMediaPaths(raw any) []any {
	var out []any
	switch v := raw.(type) {
	case []gateway.MediaRef:
		for _, m := range v {
			out = append(out, m.Path)
		}
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m["path"])
			} else {
				out = append(out, item)
			}
		}
	case []string:
		for _, s := range v {
			out = append(out, s)
		}
	}
	return out
}

// HTTP client of the standalone sender going through the proxy
func standaloneClient(proxyURL string) (*http.Client, error) {
	sessionKw, requestKw, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	transport.Proxy = nil
	if sessionKw.Connector != nil {
		transport = sessionKw.Connector.Transport()
	} else if requestKw.Proxy != "" {
		proxy, err := url.Parse(requestKw.Proxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Transport: transport}, nil
}

// Sends through the Mattermost v4 REST API without a live adapter, media uploaded first and the thread id as the root
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID := kwargs["thread_id"]
	serverURL, tokenValue, err := urlAndToken(ctx, pconfig)
	if err != nil {
		return nil, err
	}
	urlText, ok := serverURL.(string)
	if !ok {
		return nil, attributeError(serverURL, "rstrip")
	}
	tokenText, ok := tokenValue.(string)
	if !ok {
		return nil, attributeError(tokenValue, "strip")
	}
	baseURL, token := strings.TrimRight(urlText, "/"), strings.TrimSpace(tokenText)
	if baseURL == "" || token == "" {
		return gateway.SendError("Mattermost standalone send: the server address and MATTERMOST_TOKEN must both be set"), nil
	}
	uploadHeaders := map[string]string{"Authorization": "Bearer " + token}
	headers := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "MATTERMOST_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	result, err := standalonePost(ctx, proxyURL, baseURL, chatID, message, threadID, kwargs["media_files"], uploadHeaders, headers)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	shown := gateway.DescribeProxyError(err, proxyURL)
	var ce *clientError
	if errors.As(err, &ce) {
		return gateway.SendError("Mattermost send failed (network): " + cmp.Or(shown, ce.text)), nil
	}
	return gateway.SendError("Mattermost send failed: " + cmp.Or(shown, pyrepr.ExceptionMessage(err))), nil
}

// Uploads each existing media file, then creates the post with the collected file ids
func standalonePost(ctx context.Context, proxyURL, baseURL, chatID, message string, threadID, mediaFiles any, uploadHeaders, headers map[string]string) (map[string]any, error) {
	client, err := standaloneClient(proxyURL)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	fileIDs := []any{}
	for _, media := range standaloneMediaPaths(mediaFiles) {
		if !pyrepr.Truthy(media) {
			continue
		}
		filePath, ok := media.(string)
		if !ok {
			return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("stat: path should be string, bytes, os.PathLike or integer, not %s",
				pyrepr.TypeName(media))}
		}
		if _, err := os.Stat(filePath); err != nil {
			continue
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		form, formType, err := uploadForm(chatID, data, pypath.Basename(filePath), "application/octet-stream")
		if err != nil {
			return nil, err
		}
		resp, err := doRequest(ctx, client, "POST", baseURL+"/api/v4/files", uploadHeaders, formType, form, 60*time.Second)
		if err != nil {
			return nil, err
		}
		if resp.status != 200 && resp.status != 201 {
			return gateway.SendError(fmt.Sprintf("Mattermost file upload failed (%d): %s", resp.status, pystr.Slice(resp.text(), 0, 400))), nil
		}
		uploadData, err := resp.json()
		if err != nil {
			return nil, err
		}
		infos, err := dictGet(uploadData, "file_infos", []any{})
		if err != nil {
			return nil, err
		}
		list, _ := infos.([]any)
		for _, info := range list {
			id, err := dictGet(info, "id", nil)
			if err != nil {
				return nil, err
			}
			if pyrepr.Truthy(id) {
				fileIDs = append(fileIDs, id)
			}
		}
	}
	payload := pyjson.ObjectOf("channel_id", chatID, "message", message)
	if pyrepr.Truthy(threadID) {
		payload.Set("root_id", threadID)
	}
	if len(fileIDs) > 0 {
		payload.Set("file_ids", fileIDs)
	}
	body, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	resp, err := doRequest(ctx, client, "POST", baseURL+"/api/v4/posts", headers, "", []byte(body), 60*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 && resp.status != 201 {
		return gateway.SendError(fmt.Sprintf("Mattermost API error (%d): %s", resp.status, pystr.Slice(resp.text(), 0, 400))), nil
	}
	data, err := resp.json()
	if err != nil {
		return nil, err
	}
	messageID, err := dictGet(data, "id", nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "platform": "mattermost", "chat_id": chatID, "message_id": messageID}, nil
}

// Model facing description of Mattermost markdown and media delivery, kept byte stable for the cached prompt
const platformHint = "You are in a Mattermost workspace communicating with your user. Mattermost renders standard " +
	"Markdown — headings, bold, italic, code blocks, and tables all work. You can send media files " +
	"natively: include MEDIA:/absolute/path/to/file in your response. Images (.jpg, .png, .webp) are " +
	"uploaded as photo attachments, audio and video as file attachments. Image URLs in markdown format " +
	"![alt](url) are rendered as inline previews automatically."

// Whether a bot token is in the env and a server address is set
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if ok, err := gateway.EnvIsConnected("MATTERMOST_TOKEN")(ctx, cfg); err != nil || !ok {
		return false, err
	}
	return settings.MattermostURL.Get(gateway.SettingsContext(ctx)) != "", nil
}

// Mattermost platform plugin compiled into the binary under the bundled platforms/mattermost key
type mattermostPlatformPlugin struct{}

func (mattermostPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "mattermost-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/mattermost",
		Author: "Icebath",
		Description: "Mattermost gateway adapter for Icebath Agent. Connects to a self-hosted or cloud Mattermost instance via " +
			"the v4 REST API + WebSocket event stream and relays messages between Mattermost channels/DMs and the Icebath " +
			"agent. Supports thread-mode replies, native file uploads, channel-scoped allowlists, and home-channel cron " +
			"delivery.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "MATTERMOST_TOKEN", Description: "Bot account token or personal-access token", Secret: true}}}
}

// Adapter class the gateway reads the long message and serial receive traits from
var mattermostAdapterClass = &gateway.AdapterClass{Name: "MattermostAdapter", Traits: mattermostAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewMattermostAdapter(ctx, config)
	}}

// Registers the Mattermost platform entry with its factory, gates, setup, YAML bridge, env bridge, standalone sender and hint
func (mattermostPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"scoped_validate_config": validateMattermostConfig,
		"is_connected":           isConnected,
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "MATTERMOST_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxPostLength,
		"emoji":                  "💬",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "medium",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "mattermost", Label: "Mattermost",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewMattermostAdapter(fctx, cfg)
		}),
		Check:       checkMattermostRequirements,
		RequiredEnv: []string{"MATTERMOST_TOKEN"},
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(mattermostAdapterClass)
	plugins.RegisterBundled(mattermostPlatformPlugin{})
}
