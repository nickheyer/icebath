package messagetools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Slack Web API base, replaced by tests with a local server
var slackAPIBase = "https://slack.com/api/"

// Opens user and user_name targets as DM conversation ids, other ids pass through
func resolveSlackUserTarget(ctx context.Context, token any, chatID string) (string, map[string]any) {
	if !strings.HasPrefix(chatID, "user:") && !strings.HasPrefix(chatID, "user_name:") {
		return chatID, nil
	}
	id, errPayload, err := resolveSlackUserTargetOnce(ctx, token, chatID)
	if err != nil {
		return "", sendError(fmt.Sprintf("Slack DM resolution failed: %v", err))
	}
	return id, errPayload
}

func resolveSlackUserTargetOnce(ctx context.Context, token any, chatID string) (string, map[string]any, error) {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "", nil, "")
	if err != nil {
		return "", nil, err
	}
	sessKw, _, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return "", nil, err
	}
	transport := &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second}
	if sessKw.Connector != nil {
		transport = sessKw.Connector.Transport()
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	postAPI := func(method string, payload *pyjson.Object) (*pyjson.Object, error) {
		body, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		url := slackAPIBase + method
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+pyrepr.Str(token))
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
		v, err := pyjson.LoadsBytes(raw)
		if err != nil {
			return nil, err
		}
		obj, ok := v.(*pyjson.Object)
		if !ok || obj == nil {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
		}
		return obj, nil
	}
	get := func(o *pyjson.Object, k string, def any) any {
		if v, ok := o.Get(k); ok {
			return v
		}
		return def
	}
	if strings.HasPrefix(chatID, "user_name:") {
		name := chatID[len("user_name:"):]
		query := pystr.Lower(strings.TrimLeft(pystr.Strip(name), "@"))
		var matches []*pyjson.Object
		var cursor any
		for page := 0; page < 20; page++ {
			payload := pyjson.ObjectOf("limit", int64(200))
			if pyrepr.Truthy(cursor) {
				payload.Set("cursor", cursor)
			}
			data, err := postAPI("users.list", payload)
			if err != nil {
				return "", nil, err
			}
			if !pyrepr.Truthy(get(data, "ok", nil)) {
				return "", sendError(fmt.Sprintf("Slack users.list error: %s", pyrepr.Str(get(data, "error", "unknown")))), nil
			}
			members := get(data, "members", []any{})
			list, ok := members.([]any)
			if !ok {
				return "", nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(members))
			}
			for _, item := range list {
				m, ok := item.(*pyjson.Object)
				if !ok || m == nil {
					return "", nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item))
				}
				if pyrepr.Truthy(get(m, "deleted", nil)) || pyrepr.Truthy(get(m, "is_bot", nil)) {
					continue
				}
				if pystr.Lower(pystr.Strip(pyrepr.Str(get(m, "name", "")))) == query {
					matches = append(matches, m)
				}
			}
			meta := get(data, "response_metadata", nil)
			cursor = nil
			if pyrepr.Truthy(meta) {
				mo, ok := meta.(*pyjson.Object)
				if !ok {
					return "", nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(meta))
				}
				cursor = get(mo, "next_cursor", nil)
			}
			if !pyrepr.Truthy(cursor) {
				break
			}
		}
		if len(matches) == 0 {
			return "", sendError(fmt.Sprintf("Could not resolve Slack user '@%s'.", name)), nil
		}
		if len(matches) > 1 {
			return "", sendError(fmt.Sprintf("Slack user '@%s' matched multiple Slack users. Use a Slack user ID instead.", name)), nil
		}
		chatID = "user:" + pyrepr.Str(get(matches[0], "id", nil))
	}
	opened, err := postAPI("conversations.open", pyjson.ObjectOf("users", chatID[len("user:"):]))
	if err != nil {
		return "", nil, err
	}
	if !pyrepr.Truthy(get(opened, "ok", nil)) {
		return "", sendError(fmt.Sprintf("Slack conversations.open error: %s. Check bot permissions (im:write).", pyrepr.Str(get(opened, "error", "unknown")))), nil
	}
	channel := get(opened, "channel", nil)
	var dmID any
	if pyrepr.Truthy(channel) {
		co, ok := channel.(*pyjson.Object)
		if !ok {
			return "", nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(channel))
		}
		dmID = get(co, "id", nil)
	}
	if !pyrepr.Truthy(dmID) {
		return "", sendError("Slack conversations.open did not return a DM channel ID"), nil
	}
	return pyrepr.Str(dmID), nil, nil
}
