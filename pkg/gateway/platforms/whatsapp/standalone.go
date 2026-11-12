package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Extension to bridge media kind for out of process sends
var waExtMediaType = map[string]string{
	".jpg": "image", ".jpeg": "image", ".png": "image", ".webp": "image", ".gif": "image",
	".mp4": "video", ".mov": "video", ".avi": "video", ".mkv": "video", ".webm": "video", ".3gp": "video",
	".ogg": "audio", ".opus": "audio", ".mp3": "audio", ".wav": "audio", ".m4a": "audio", ".flac": "audio",
}

// Bridge mediaType of a local file, document when forced and audio for a voice note
func bridgeMediaType(filePath string, isVoice, forceDocument bool) string {
	switch {
	case forceDocument:
		return "document"
	case isVoice:
		return "audio"
	}
	if kind, ok := waExtMediaType[strings.ToLower(filepath.Ext(filePath))]; ok {
		return kind
	}
	return "document"
}

// Transport of out of process sends, the default transport when nil
var standaloneTransport http.RoundTripper

// Valid participant JIDs of the mentions, deduplicated in order
func normalizeOutboundMentions(mentions []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range mentions {
		if jid := gateway.NormalizeWhatsAppMentionJID(m); jid != "" && !seen[jid] {
			seen[jid] = true
			out = append(out, jid)
		}
	}
	return out
}

// Out of process delivery through this profile's bridge HTTP API, text first then each media file natively
func standaloneSend(ctx context.Context, kwargs map[string]any) (result map[string]any, err error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	extra := map[string]any{}
	if pconfig != nil && pconfig.Extra != nil {
		extra = pconfig.Extra
	}
	message, _ := kwargs["message"].(string)
	media, _ := kwargs["media_files"].([]gateway.MediaRef)
	forceDocument, _ := kwargs["force_document"].(bool)
	caption, _ := kwargs["caption"].(string)
	mentions, _ := kwargs["mentions"].([]string)
	bridgePort, err := StandaloneBridgePort(home.Dir(ctx), extra["bridge_port"])
	if err != nil {
		return gateway.SendError("WhatsApp send failed: " + err.Error()), nil
	}
	ownSession := sessionDir(ctx, extra)
	normalizedChatID := gateway.ToWhatsAppJID(pyrepr.Str(kwargs["chat_id"]))
	mediaCaption := ""
	if caption != "" && len(media) == 1 {
		mediaCaption = caption
	}
	pending := normalizeOutboundMentions(mentions)
	mentionFirst := func(payload map[string]any) map[string]any {
		if len(pending) > 0 {
			payload["mentions"] = pending
			pending = nil
		}
		return payload
	}
	client := &http.Client{Transport: standaloneTransport}
	defer client.CloseIdleConnections()
	request := func(method, path string, payload any, total time.Duration) (int, []byte, error) {
		rctx, cancel := context.WithTimeout(ctx, total)
		defer cancel()
		var body io.Reader
		if payload != nil {
			raw, err := json.Marshal(payload)
			if err != nil {
				return 0, nil, err
			}
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(rctx, method, fmt.Sprintf("http://localhost:%d/%s", bridgePort, path), body)
		if err != nil {
			return 0, nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, err
	}
	failed := func(err error) (map[string]any, error) {
		return gateway.SendError("WhatsApp send failed: " + err.Error()), nil
	}
	status, raw, err := request(http.MethodGet, "health", nil, 5*time.Second)
	if err != nil {
		return failed(err)
	}
	if status != 200 {
		return gateway.SendError(fmt.Sprintf("WhatsApp bridge on port %d answered /health with HTTP %d; nothing was sent.", bridgePort, status)), nil
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return failed(err)
	}
	health, ok := decoded.(*pyjson.Object)
	if !ok {
		return failed(fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(decoded)))
	}
	if reported := health.GetOr("session", nil); pyrepr.Truthy(reported) && pyrepr.Str(reported) != ownSession {
		return gateway.SendError(fmt.Sprintf("WhatsApp bridge on port %d serves another profile's session (%s); "+
			"nothing was sent. Give each profile its own platforms.whatsapp.extra.bridge_port.", bridgePort, pyrepr.Str(reported))), nil
	}
	if len(pending) > 0 {
		caps, _ := health.GetOr("capabilities", nil).(*pyjson.Object)
		if caps == nil || !pyrepr.Truthy(caps.GetOr("outboundMentions", nil)) {
			return map[string]any{"error": "WhatsApp bridge does not support native mentions; restart it from the same Icebath version."}, nil
		}
	}
	post := func(path string, payload map[string]any, total time.Duration, label string) (any, map[string]any, error) {
		status, raw, err := request(http.MethodPost, path, payload, total)
		if err != nil {
			return nil, nil, err
		}
		if status == 200 {
			data, err := pyjson.LoadsBytes(raw)
			if err != nil {
				return nil, nil, err
			}
			if obj, ok := data.(*pyjson.Object); ok {
				return obj.GetOr("messageId", nil), nil, nil
			}
			return nil, nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(data))
		}
		if label == "" {
			return nil, map[string]any{}, nil
		}
		return nil, gateway.SendError(fmt.Sprintf("WhatsApp %s error (%d): %s", label, status, string(raw))), nil
	}
	var lastMessageID any
	if pystr.Strip(message) != "" && mediaCaption == "" {
		id, errPayload, err := post("send", mentionFirst(map[string]any{"chatId": normalizedChatID, "message": message}), 30*time.Second, "bridge")
		if err != nil {
			return failed(err)
		}
		if len(errPayload) > 0 {
			return errPayload, nil
		}
		lastMessageID = id
	}
	for _, item := range media {
		if _, statErr := os.Stat(item.Path); statErr != nil {
			if mediaCaption != "" {
				if _, _, err := post("send", mentionFirst(map[string]any{"chatId": normalizedChatID, "message": mediaCaption}), 30*time.Second, ""); err != nil {
					slog.Warn("WhatsApp caption-fallback send failed for missing media")
				}
			}
			return gateway.SendError("WhatsApp media file not found: " + item.Path), nil
		}
		mediaType := bridgeMediaType(item.Path, item.IsVoice, forceDocument)
		payload := map[string]any{"chatId": normalizedChatID, "filePath": item.Path, "mediaType": mediaType}
		if mediaType == "document" {
			payload["fileName"] = filepath.Base(item.Path)
		}
		if mediaCaption != "" {
			payload["caption"] = mediaCaption
		}
		id, errPayload, err := post("send-media", mentionFirst(payload), 120*time.Second, "media")
		if err != nil {
			return failed(err)
		}
		if len(errPayload) > 0 {
			return errPayload, nil
		}
		if pyrepr.Truthy(id) {
			lastMessageID = id
		}
	}
	return map[string]any{"success": true, "platform": "whatsapp", "chat_id": normalizedChatID, "message_id": lastMessageID}, nil
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case int64:
		return "int"
	case float64:
		return "float"
	case nil:
		return "NoneType"
	}
	return "dict"
}

// config.yaml whatsapp keys bridged to WHATSAPP env, env winning
var yamlBridgeRows = []gateway.YAMLBridgeRow{
	{Key: "require_mention", Env: "WHATSAPP_REQUIRE_MENTION", Kind: "lower"}, {Key: "dm_policy", Env: "WHATSAPP_DM_POLICY", Kind: "lower"},
	{Key: "group_policy", Env: "WHATSAPP_GROUP_POLICY", Kind: "lower"}, {Key: "mention_patterns", Env: "WHATSAPP_MENTION_PATTERNS", Kind: "json"},
	{Key: "free_response_chats", Env: "WHATSAPP_FREE_RESPONSE_CHATS", Kind: "csv"}, {Key: "allow_from", Env: "WHATSAPP_ALLOWED_USERS", Kind: "csv"},
	{Key: "group_allow_from", Env: "WHATSAPP_GROUP_ALLOWED_USERS", Kind: "csv"},
}

// Bridges config.yaml whatsapp keys into the WHATSAPP env and the platform extra
func applyYAMLConfig(ctx context.Context, yamlCfg, whatsappCfg map[string]any) (map[string]any, error) {
	return gateway.ApplyYAMLBridge(ctx, whatsappCfg, yamlBridgeRows)
}
