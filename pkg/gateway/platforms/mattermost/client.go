package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	requestTimeout = 30 * time.Second
	uploadTimeout  = 60 * time.Second
)

// Failed REST call with its HTTP status, zero for a network failure, and the response body or error text
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	if e.status == 0 {
		return e.body
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, clip(e.body, 200))
}

// Status and body of a failed call, zero and empty for other errors
func failureOf(err error) (int, string) {
	if e, ok := err.(*apiError); ok {
		return e.status, e.body
	}
	if err != nil {
		return 0, err.Error()
	}
	return 0, ""
}

// First n bytes of s cut at a rune boundary
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Text form of a config or JSON value, empty for nil
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

// Whether a config or metadata value is set to something other than false, zero or empty
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	default:
		return true
	}
}

// Server URL and token from the platform config, each falling back to its scoped env var
func urlAndToken(ctx context.Context, config *gateway.PlatformConfig) (string, string, error) {
	var baseURL, token string
	if config != nil {
		baseURL = textOf(config.Extra["url"])
		token = textOf(config.Token)
	}
	var err error
	if baseURL == "" {
		if baseURL, err = gateway.GetScopedSecret(ctx, "MATTERMOST_URL", "", false); err != nil {
			return "", "", err
		}
	}
	if token == "" {
		if token, err = gateway.GetScopedSecret(ctx, "MATTERMOST_TOKEN", "", false); err != nil {
			return "", "", err
		}
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/"), strings.TrimSpace(token), nil
}

// Mime type of a file name from its extension, the fallback when the extension is unknown
func guessMime(name, fallback string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		if i := strings.IndexByte(t, ';'); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		return t
	}
	return fallback
}

// Status and body of one bearer authenticated request bounded by the timeout
func doRequest(ctx context.Context, client *http.Client, method, rawURL, token, contentType string, body io.Reader, timeout time.Duration) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, rawURL, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// Decoded JSON object of one v4 call, an apiError for a traversal path, an HTTP failure or a network failure
func callJSON(ctx context.Context, client *http.Client, baseURL, token, method, path string, payload any) (map[string]any, error) {
	if strings.Contains(path, "..") {
		return nil, &apiError{body: "path traversal blocked: " + path}
	}
	var body io.Reader
	contentType := "application/json"
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	status, data, err := doRequest(ctx, client, method, baseURL+"/api/v4/"+strings.TrimLeft(path, "/"), token, contentType, body, requestTimeout)
	if err != nil {
		return nil, &apiError{status: status, body: err.Error()}
	}
	if status >= 400 {
		return nil, &apiError{status: status, body: string(data)}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &apiError{status: status, body: "invalid JSON response: " + err.Error()}
	}
	return out, nil
}

// File ids the server assigns to one multipart upload attributed to the channel
func uploadFile(ctx context.Context, client *http.Client, baseURL, token, channelID string, data []byte, filename, contentType string) ([]string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("channel_id", channelID); err != nil {
		return nil, err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, quoteEscaper.Replace(filename)))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	status, body, err := doRequest(ctx, client, http.MethodPost, baseURL+"/api/v4/files", token, w.FormDataContentType(), &buf, uploadTimeout)
	if err != nil {
		return nil, &apiError{status: status, body: err.Error()}
	}
	if status < 200 || status >= 300 {
		return nil, &apiError{status: status, body: string(body)}
	}
	var resp struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &apiError{status: status, body: "invalid JSON response: " + err.Error()}
	}
	var ids []string
	for _, info := range resp.FileInfos {
		if info.ID != "" {
			ids = append(ids, info.ID)
		}
	}
	return ids, nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// Post payload with a props block that stops the server firing mentions
func mentionsDisabled(payload map[string]any) map[string]any {
	payload["props"] = map[string]any{"disable_mentions": true}
	return payload
}
