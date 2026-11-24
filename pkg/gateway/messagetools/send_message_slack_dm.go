package messagetools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Slack Web API base, replaced by tests with a local server
var slackAPIBase = "https://slack.com/api/"

// Opens user and user_name targets as DM conversation ids, other ids pass through
func resolveSlackUserTarget(ctx context.Context, token any, chatID string) (string, map[string]any) {
	if !strings.HasPrefix(chatID, "user:") && !strings.HasPrefix(chatID, "user_name:") {
		return chatID, nil
	}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "", nil, "")
	if err != nil {
		return "", sendError(fmt.Sprintf("Slack DM resolution failed: %v", err))
	}
	id, errPayload, err := resolveSlackUserTargetOnce(ctx, token, chatID, proxyURL)
	if err != nil {
		return "", sendError("Slack DM resolution failed: " + gateway.ProxySafeErrorText(err, proxyURL))
	}
	return id, errPayload
}

func resolveSlackUserTargetOnce(ctx context.Context, token any, chatID, proxyURL string) (string, map[string]any, error) {
	sessKw, _, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return "", nil, err
	}
	transport := &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second}
	if sessKw.Connector != nil {
		transport = sessKw.Connector.Transport()
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	postAPI := func(method string, payload *jsonutil.Object) (*jsonutil.Object, error) {
		body, err := jsonutil.Encode(payload)
		if err != nil {
			return nil, err
		}
		url := slackAPIBase + method
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+anyutil.String(token))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
			return nil, fmt.Errorf("%d, message='Attempt to decode JSON with unexpected mimetype: %s', url='%s'", resp.StatusCode, mt, url)
		}
		v, err := jsonutil.Decode(raw)
		if err != nil {
			return nil, err
		}
		obj, ok := v.(*jsonutil.Object)
		if !ok || obj == nil {
			return nil, fmt.Errorf("the Slack reply must be a JSON object, got %s", errchain.Kind(v))
		}
		return obj, nil
	}
	get := func(o *jsonutil.Object, k string, def any) any {
		if v, ok := o.Get(k); ok {
			return v
		}
		return def
	}
	if strings.HasPrefix(chatID, "user_name:") {
		name := chatID[len("user_name:"):]
		query := strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "@"))
		var matches []*jsonutil.Object
		var cursor any
		for page := 0; page < 20; page++ {
			payload := jsonutil.ObjectOf("limit", int64(200))
			if anyutil.IsTrue(cursor) {
				payload.Set("cursor", cursor)
			}
			data, err := postAPI("users.list", payload)
			if err != nil {
				return "", nil, err
			}
			if !anyutil.IsTrue(get(data, "ok", nil)) {
				return "", sendError(fmt.Sprintf("Slack users.list error: %s", anyutil.String(get(data, "error", "unknown")))), nil
			}
			members := get(data, "members", []any{})
			list, ok := members.([]any)
			if !ok {
				return "", nil, fmt.Errorf("the Slack users.list members must be a list, got %s", errchain.Kind(members))
			}
			for _, item := range list {
				m, ok := item.(*jsonutil.Object)
				if !ok || m == nil {
					return "", nil, fmt.Errorf("a Slack users.list member must be a mapping, got %s", errchain.Kind(item))
				}
				if anyutil.IsTrue(get(m, "deleted", nil)) || anyutil.IsTrue(get(m, "is_bot", nil)) {
					continue
				}
				if strings.ToLower(strings.TrimSpace(anyutil.String(get(m, "name", "")))) == query {
					matches = append(matches, m)
				}
			}
			meta := get(data, "response_metadata", nil)
			cursor = nil
			if anyutil.IsTrue(meta) {
				mo, ok := meta.(*jsonutil.Object)
				if !ok {
					return "", nil, fmt.Errorf("the Slack users.list response_metadata must be a mapping, got %s", errchain.Kind(meta))
				}
				cursor = get(mo, "next_cursor", nil)
			}
			if !anyutil.IsTrue(cursor) {
				break
			}
		}
		if len(matches) == 0 {
			return "", sendError(fmt.Sprintf("Could not resolve Slack user '@%s'.", name)), nil
		}
		if len(matches) > 1 {
			return "", sendError(fmt.Sprintf("Slack user '@%s' matched multiple Slack users. Use a Slack user ID instead.", name)), nil
		}
		chatID = "user:" + anyutil.String(get(matches[0], "id", nil))
	}
	opened, err := postAPI("conversations.open", jsonutil.ObjectOf("users", chatID[len("user:"):]))
	if err != nil {
		return "", nil, err
	}
	if !anyutil.IsTrue(get(opened, "ok", nil)) {
		return "", sendError(fmt.Sprintf("Slack conversations.open error: %s. Check bot permissions (im:write).", anyutil.String(get(opened, "error", "unknown")))), nil
	}
	channel := get(opened, "channel", nil)
	var dmID any
	if anyutil.IsTrue(channel) {
		co, ok := channel.(*jsonutil.Object)
		if !ok {
			return "", nil, fmt.Errorf("the Slack conversations.open channel must be a mapping, got %s", errchain.Kind(channel))
		}
		dmID = get(co, "id", nil)
	}
	if !anyutil.IsTrue(dmID) {
		return "", sendError("Slack conversations.open did not return a DM channel ID"), nil
	}
	return anyutil.String(dmID), nil, nil
}
