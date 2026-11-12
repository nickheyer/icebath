package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	discordStandaloneJSONBodyLimitBytes  = 1 * 1024 * 1024
	discordStandaloneErrorBodyLimitBytes = 8 * 1024
)

// Base URL of the Discord REST API the standalone sender posts to
var discordStandaloneAPIBase = "https://discord.com/api/v10"

// Process local channel type probe cache, chat id to forum flag
var (
	channelTypeProbeMu    sync.Mutex
	channelTypeProbeCache = map[string]bool{}
)

// Remembers whether a chat id is a forum channel
func rememberChannelIsForum(chatID string, isForum bool) {
	channelTypeProbeMu.Lock()
	channelTypeProbeCache[chatID] = isForum
	channelTypeProbeMu.Unlock()
}

// Cached forum flag of a chat id, ok false when never probed
func probeIsForumCached(chatID string) (bool, bool) {
	channelTypeProbeMu.Lock()
	defer channelTypeProbeMu.Unlock()
	v, ok := channelTypeProbeCache[chatID]
	return v, ok
}

// At most limit bytes of the body and whether more remained, closing the response when cut
func standaloneReadResponseBytesLimited(resp *http.Response, limit int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > limit {
		resp.Body.Close()
		return body[:limit], true, nil
	}
	return body, false, nil
}

// Charset the response declares, utf-8 when absent
func standaloneResponseEncoding(resp *http.Response) string {
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err == nil && params["charset"] != "" {
		return params["charset"]
	}
	return "utf-8"
}

// Bytes decoded with the response charset, invalid sequences replaced
func standaloneDecode(resp *http.Response, body []byte) string {
	text, err := pycodecs.DecodeReplace(body, standaloneResponseEncoding(resp))
	if err != nil {
		text, _ = pycodecs.DecodeReplace(body, "utf-8")
	}
	return text
}

// Body text read within the byte limit
func standaloneReadTextLimited(resp *http.Response, limit int) (string, error) {
	body, _, err := standaloneReadResponseBytesLimited(resp, limit)
	if err != nil {
		return "", err
	}
	return standaloneDecode(resp, body), nil
}

// JSON object body within the byte limit, empty for an empty body or a non object
func standaloneReadJSONLimited(resp *http.Response, limit int) (map[string]any, error) {
	body, truncated, err := standaloneReadResponseBytesLimited(resp, limit)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("Discord API JSON response exceeds %d bytes", limit)
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	data, err := pyjson.Loads(standaloneDecode(resp, body))
	if err != nil {
		return nil, err
	}
	if obj, ok := pyjsonPlain(data).(map[string]any); ok {
		return obj, nil
	}
	return map[string]any{}, nil
}

// Logs and returns the warning for a missing media file
func standaloneWarnMissingMedia(mediaPath string) string {
	warning := fmt.Sprintf("Media file not found, skipping: %s", mediaPath)
	slog.Warn(warning)
	return warning
}

// JSON body of a 200 or 201 response, else the error payload carrying the capped body text
func standaloneResponseJSONOrError(resp *http.Response, errorPrefix string) (map[string]any, map[string]any, error) {
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		body, err := standaloneReadTextLimited(resp, discordStandaloneErrorBodyLimitBytes)
		if err != nil {
			return nil, nil, err
		}
		return nil, gateway.SendError(fmt.Sprintf("%s (%d): %s", errorPrefix, resp.StatusCode, body)), nil
	}
	data, err := standaloneReadJSONLimited(resp, discordStandaloneJSONBodyLimitBytes)
	return data, nil, err
}

// HTTP client of the standalone sender with the given timeout and proxy settings
func standaloneClient(session gateway.ProxyKwargs, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if session.Connector != nil {
		client.Transport = session.Connector.Transport()
	}
	return client
}

// Sends one request and returns the response
func standaloneDo(ctx context.Context, client *http.Client, method, url string, header http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	return client.Do(req)
}

// Whether the chat is a forum, by channel directory, then probe cache, then a memoized channel fetch
func standaloneIsForum(ctx context.Context, chatID string, jsonHeaders http.Header, session gateway.ProxyKwargs) bool {
	if kind, err := gateway.LookupChannelType(ctx, "discord", chatID); err == nil && kind != nil {
		return kind == "forum"
	}
	if cached, ok := probeIsForumCached(chatID); ok {
		return cached
	}
	isForum := false
	resp, err := standaloneDo(ctx, standaloneClient(session, 15*time.Second), http.MethodGet, discordStandaloneAPIBase+"/channels/"+chatID, jsonHeaders, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to probe channel type for %s", chatID), "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		info, err := standaloneReadJSONLimited(resp, discordStandaloneJSONBodyLimitBytes)
		if err != nil {
			slog.Debug(fmt.Sprintf("Failed to probe channel type for %s", chatID), "err", err)
			return false
		}
		isForum = reflect.DeepEqual(info["type"], int64(15))
		rememberChannelIsForum(chatID, isForum)
	}
	return isForum
}

// Multipart body with an optional payload_json part and file parts
func multipartBody(payload any, files map[string]string, order []string) (string, *bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	if payload != nil {
		text, err := pyjson.Dumps(payload)
		if err != nil {
			return "", nil, err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="payload_json"`)
		h.Set("Content-Type", "application/json")
		part, err := w.CreatePart(h)
		if err != nil {
			return "", nil, err
		}
		io.WriteString(part, text)
	}
	for _, field := range order {
		path := files[field]
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		part, err := w.CreateFormFile(field, filepath.Base(path))
		if err != nil {
			return "", nil, err
		}
		part.Write(data)
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf, nil
}

// Posts JSON and returns the response
func standalonePostJSON(ctx context.Context, client *http.Client, url string, headers http.Header, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return standaloneDo(ctx, client, http.MethodPost, url, headers, bytes.NewReader(raw))
}

// Media path and voice flag pairs of the media_files argument
func standaloneMediaFiles(raw any) [][2]any {
	var out [][2]any
	switch v := raw.(type) {
	case []gateway.MediaRef:
		for _, m := range v {
			out = append(out, [2]any{m.Path, m.IsVoice})
		}
	case []any:
		for _, item := range v {
			if pair, ok := item.([]any); ok && len(pair) == 2 {
				out = append(out, [2]any{pyrepr.Str(pair[0]), pair[1]})
			}
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Sends through the Discord REST API without a live gateway adapter, forum channels getting a thread post
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID := pyrepr.Str(kwargs["chat_id"])
	message, _ := kwargs["message"].(string)
	threadID := ""
	if v := kwargs["thread_id"]; v != nil {
		threadID = pyrepr.Str(v)
	}
	caption := ""
	if v := kwargs["caption"]; v != nil {
		caption = pyrepr.Str(v)
	}
	mediaFiles := standaloneMediaFiles(kwargs["media_files"])
	token := ""
	if pconfig != nil && pconfig.Token != nil {
		token = pystr.Strip(pyrepr.Str(pconfig.Token))
	}
	if token == "" {
		secret, _ := gateway.GetScopedSecret(ctx, "DISCORD_BOT_TOKEN", "", false)
		token = pystr.Strip(secret)
	}
	if token == "" {
		return gateway.SendError("Discord standalone send: DISCORD_BOT_TOKEN is not set"), nil
	}
	result, err := standaloneSendWithToken(ctx, token, chatID, message, threadID, caption, mediaFiles)
	if err != nil {
		slog.Error("Discord standalone send failed", "err", err)
		return gateway.SendError(fmt.Sprintf("Discord send failed: %s: %s", pyErrorTypeName(err), err)), nil
	}
	return result, nil
}

// Python exception class name of a send failure, timeouts and connection failures named as aiohttp does
func pyErrorTypeName(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "TimeoutError"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "ClientConnectorError"
	}
	return "ClientError"
}

// Body of the standalone send once the token is known, errors surfacing as the caller's send failure
func standaloneSendWithToken(ctx context.Context, token, chatID, message, threadID, caption string, mediaFiles [][2]any) (map[string]any, error) {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "DISCORD_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	session, _, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return nil, err
	}
	authHeaders := http.Header{"Authorization": {"Bot " + token}}
	jsonHeaders := authHeaders.Clone()
	jsonHeaders.Set("Content-Type", "application/json")
	var lastData map[string]any
	var warnings []any
	var url string
	if threadID != "" {
		url = discordStandaloneAPIBase + "/channels/" + threadID + "/messages"
	} else {
		if standaloneIsForum(ctx, chatID, jsonHeaders, session) {
			threadName := deriveForumThreadName(ctx, message)
			threadURL := discordStandaloneAPIBase + "/channels/" + chatID + "/threads"
			var valid []string
			for _, m := range mediaFiles {
				path := m[0].(string)
				if !exists(path) {
					warnings = append(warnings, standaloneWarnMissingMedia(path))
					continue
				}
				valid = append(valid, path)
			}
			client := standaloneClient(session, 60*time.Second)
			var data, errPayload map[string]any
			if len(valid) > 0 {
				meta := []any{}
				files, order := map[string]string{}, []string{}
				for idx, path := range valid {
					meta = append(meta, map[string]any{"id": fmt.Sprint(idx), "filename": filepath.Base(path)})
					field := fmt.Sprintf("files[%d]", idx)
					files[field] = path
					order = append(order, field)
				}
				content := caption
				if content == "" {
					content = message
				}
				payload := map[string]any{"name": threadName, "message": map[string]any{"content": content, "attachments": meta}}
				uploadErr := func() error {
					contentType, body, err := multipartBody(payload, files, order)
					if err != nil {
						return err
					}
					headers := authHeaders.Clone()
					headers.Set("Content-Type", contentType)
					resp, err := standaloneDo(ctx, client, http.MethodPost, threadURL, headers, body)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					data, errPayload, err = standaloneResponseJSONOrError(resp, "Discord forum thread creation error")
					return err
				}()
				if uploadErr != nil {
					return gateway.SendError(fmt.Sprintf("Discord forum thread upload failed: %s", uploadErr)), nil
				}
			} else {
				resp, err := standalonePostJSON(ctx, client, threadURL, jsonHeaders, map[string]any{"name": threadName, "message": map[string]any{"content": message}})
				if err != nil {
					return nil, err
				}
				data, errPayload, err = standaloneResponseJSONOrError(resp, "Discord forum thread creation error")
				resp.Body.Close()
				if err != nil {
					return nil, err
				}
			}
			if errPayload != nil {
				return errPayload, nil
			}
			created := data["id"]
			starter := created
			if msg, ok := data["message"].(map[string]any); ok {
				if id, has := msg["id"]; has {
					starter = id
				}
			}
			result := map[string]any{"success": true, "platform": "discord", "chat_id": chatID, "thread_id": created, "message_id": starter}
			if len(warnings) > 0 {
				result["warnings"] = warnings
			}
			return result, nil
		}
		url = discordStandaloneAPIBase + "/channels/" + chatID + "/messages"
	}
	client := standaloneClient(session, 30*time.Second)
	if pystr.Strip(message) != "" || len(mediaFiles) == 0 {
		resp, err := standalonePostJSON(ctx, client, url, jsonHeaders, map[string]any{"content": message})
		if err != nil {
			return nil, err
		}
		data, errPayload, err := standaloneResponseJSONOrError(resp, "Discord API error")
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if errPayload != nil {
			return errPayload, nil
		}
		lastData = data
	}
	captionPending := caption != ""
	for _, m := range mediaFiles {
		path := m[0].(string)
		if !exists(path) {
			warnings = append(warnings, standaloneWarnMissingMedia(path))
			if captionPending {
				resp, err := standalonePostJSON(ctx, client, url, jsonHeaders, map[string]any{"content": caption})
				if err != nil {
					slog.Warn("Discord caption-fallback send failed for missing media")
					continue
				}
				if resp.StatusCode == 200 || resp.StatusCode == 201 {
					if data, err := standaloneReadJSONLimited(resp, discordStandaloneJSONBodyLimitBytes); err == nil {
						lastData = data
						captionPending = false
					} else {
						slog.Warn("Discord caption-fallback send failed for missing media")
					}
				}
				resp.Body.Close()
			}
			continue
		}
		mediaErr := func() error {
			var payload any
			if captionPending {
				payload = map[string]any{"content": caption}
				captionPending = false
			}
			contentType, body, err := multipartBody(payload, map[string]string{"files[0]": path}, []string{"files[0]"})
			if err != nil {
				return err
			}
			headers := authHeaders.Clone()
			headers.Set("Content-Type", contentType)
			resp, err := standaloneDo(ctx, client, http.MethodPost, url, headers, body)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			data, errPayload, err := standaloneResponseJSONOrError(resp, "Discord API error")
			if err != nil {
				return err
			}
			if errPayload != nil {
				warning := gateway.SendError(fmt.Sprintf("Failed to send media %s: %s", path, pyrepr.Str(errPayload["error"])))["error"]
				slog.Error(pyrepr.Str(warning))
				warnings = append(warnings, warning)
				return nil
			}
			lastData = data
			return nil
		}()
		if mediaErr != nil {
			warning := gateway.SendError(fmt.Sprintf("Failed to send media %s: %s", path, mediaErr))["error"]
			slog.Error(pyrepr.Str(warning))
			warnings = append(warnings, warning)
		}
	}
	if lastData == nil {
		out := gateway.SendError("No deliverable text or media remained after processing")
		if len(warnings) > 0 {
			out["warnings"] = warnings
		}
		return out, nil
	}
	result := map[string]any{"success": true, "platform": "discord", "chat_id": chatID, "message_id": lastData["id"]}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
}
