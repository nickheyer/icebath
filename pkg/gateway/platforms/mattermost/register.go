package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const standaloneTimeout = 60 * time.Second

// Config keys bridged to env, allowed channels acting as a whitelist
var yamlBridge = []gateway.YAMLBridgeRow{
	{Key: "require_mention", Env: "MATTERMOST_REQUIRE_MENTION", Kind: "lower"},
	{Key: "free_response_channels", Env: "MATTERMOST_FREE_RESPONSE_CHANNELS", Kind: "csv"},
	{Key: "allowed_channels", Env: "MATTERMOST_ALLOWED_CHANNELS", Kind: "csv"},
}

// Seeds the mattermost config keys into extra and bridges them to env without overriding an explicit env
func applyYAMLConfig(ctx context.Context, yamlCfg, block map[string]any) (map[string]any, error) {
	return gateway.ApplyYAMLBridge(ctx, block, yamlBridge)
}

// Whether the config or the scoped env carries both the server URL and the token
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	baseURL, token, err := urlAndToken(ctx, config)
	if err != nil {
		return false, err
	}
	if token == "" {
		slog.Debug("Mattermost: MATTERMOST_TOKEN not set")
		return false, nil
	}
	if baseURL == "" {
		slog.Warn("Mattermost: MATTERMOST_URL not set")
		return false, nil
	}
	return true, nil
}

// Local paths of the standalone media argument, plain paths or path mappings
func mediaPaths(raw any) []string {
	var out []string
	add := func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case gateway.MediaRef:
			out = append(out, x.Path)
		case map[string]any:
			out = append(out, textOf(x["path"]))
		}
	}
	switch list := raw.(type) {
	case []gateway.MediaRef:
		for _, m := range list {
			out = append(out, m.Path)
		}
	case []string:
		out = append(out, list...)
	case []any:
		for _, item := range list {
			add(item)
		}
	}
	return out
}

// HTTP client of the standalone sender over MATTERMOST_PROXY or the trusted proxy env
func standaloneClient(ctx context.Context) (*http.Client, error) {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "MATTERMOST_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	if proxyURL == "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		return &http.Client{Timeout: standaloneTimeout, Transport: transport}, nil
	}
	connector, err := gateway.NewProxyConnector(proxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: standaloneTimeout, Transport: connector.Transport()}, nil
}

// Out of process delivery through the REST API, media uploaded and attached and the thread id used as the root
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID, _ := kwargs["thread_id"].(string)
	baseURL, token, err := urlAndToken(ctx, pconfig)
	if err != nil {
		return gateway.SendError("Mattermost send failed: " + err.Error()), nil
	}
	if baseURL == "" || token == "" {
		return gateway.SendError("Mattermost standalone send: MATTERMOST_URL and MATTERMOST_TOKEN must both be set"), nil
	}
	client, err := standaloneClient(ctx)
	if err != nil {
		return gateway.SendError("Mattermost send failed: " + err.Error()), nil
	}
	defer client.CloseIdleConnections()
	var fileIDs []string
	skipped := 0
	for _, path := range mediaPaths(kwargs["media_files"]) {
		data, err := os.ReadFile(path)
		if path == "" || err != nil {
			skipped++
			continue
		}
		name := filepath.Base(path)
		ids, err := uploadFile(ctx, client, baseURL, token, chatID, data, name, guessMime(name, "application/octet-stream"))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			status, body := failureOf(err)
			if status == 0 {
				return gateway.SendError("Mattermost send failed (network): " + body), nil
			}
			return gateway.SendError(fmt.Sprintf("Mattermost file upload failed (%d): %s", status, clip(body, 400))), nil
		}
		fileIDs = append(fileIDs, ids...)
	}
	payload := mentionsDisabled(map[string]any{"channel_id": chatID, "message": message})
	if threadID != "" {
		payload["root_id"] = threadID
	}
	if len(fileIDs) > 0 {
		payload["file_ids"] = fileIDs
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return gateway.SendError("Mattermost send failed: " + err.Error()), nil
	}
	status, body, err := doRequest(ctx, client, http.MethodPost, baseURL+"/api/v4/posts", token, "application/json", bytes.NewReader(raw), standaloneTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("Mattermost send failed (network): " + err.Error()), nil
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return gateway.SendError(fmt.Sprintf("Mattermost API error (%d): %s", status, clip(string(body), 400))), nil
	}
	var post map[string]any
	if err := json.Unmarshal(body, &post); err != nil {
		return gateway.SendError("Mattermost send failed: invalid JSON response: " + err.Error()), nil
	}
	result := map[string]any{"success": true, "platform": "mattermost", "chat_id": chatID, "message_id": post["id"]}
	if len(fileIDs) > 0 && skipped == 0 {
		result["media_delivered"] = true
	}
	return result, nil
}

// Model facing description of Mattermost rendering and media delivery
const platformHint = "You are in a Mattermost workspace communicating with your user. Mattermost renders standard " +
	"Markdown — headings, bold, italic, code blocks, and tables all work. You can send media files " +
	"natively: include MEDIA:/absolute/path/to/file in your response. Images (.jpg, .png, .webp) are " +
	"uploaded as photo attachments, audio and video as file attachments. Image URLs in markdown format " +
	"![alt](url) are rendered as inline previews automatically."

// Mattermost platform plugin compiled into the binary under the bundled platforms/mattermost key
type platformPlugin struct{}

func (platformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "mattermost-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/mattermost",
		Author: "heyer.app",
		Description: "Mattermost gateway adapter for Icebath. Connects to a self-hosted or cloud Mattermost instance through the v4 " +
			"REST API and the websocket event stream and relays messages between Mattermost channels and direct messages and the " +
			"Icebath agent. Supports thread replies, native file uploads, channel allowlists and home channel cron delivery.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "MATTERMOST_URL", Description: "Mattermost server URL (e.g. https://mm.example.com)"},
			{Name: "MATTERMOST_TOKEN", Description: "Bot account token or personal-access token", Secret: true}}}
}

// Registers the Mattermost platform with its factory, gates, wizard, config bridge, standalone sender and hint
func (platformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"dashboard_description":  "Connect Icebath to Mattermost channels and direct messages.",
		"docs_url":               "https://mattermost.com/deploy/",
		"dashboard_env_vars":     []string{"MATTERMOST_URL", "MATTERMOST_TOKEN", "MATTERMOST_ALLOWED_USERS"},
		"scoped_validate_config": validateConfig,
		"is_connected":           gateway.EnvIsConnected("MATTERMOST_TOKEN", "MATTERMOST_URL"),
		"setup_fn":               InteractiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
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
			return NewAdapter(fctx, cfg)
		}),
		Check:       func() bool { return true },
		RequiredEnv: []string{"MATTERMOST_URL", "MATTERMOST_TOKEN"},
		Extra:       extra,
	})
	return err
}

func init() {
	plugins.RegisterBundled(platformPlugin{})
}
